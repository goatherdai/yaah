// control_driver.go holds the cmd-side half of the control-plane bridge in
// internal/control: the transport adapters that turn a surface's channel or
// SSE stream into a control.Surface, and the installer that wires the shared
// question and approval bridges onto a session.
//
// The prompt policy itself — what is sent, how long a round-trip may take,
// what happens when nobody answers — lives in internal/control, so the TUI,
// the web UI and the ACP server cannot each grow their own copy of it.
package yaah

import (
	"context"
	"time"

	"github.com/goatherdai/yaah/internal/control"
	"github.com/goatherdai/yaah/internal/toolcontract"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/workflow/exec"
)

// sendCtrlMsg delivers one control-plane prompt onto a consumer's channel,
// bounded by wait and, when done is non-nil, by that consumer going away. It
// reports false when the prompt could not be handed over, which is the caller's
// cue to fall back instead of waiting. Callers pick the bound: the bridge's
// control.SendTimeout for a surface that may be slow to attach, a shorter one
// for a channel that is drained for the life of the process.
func sendCtrlMsg(ch chan<- control.Msg, done <-chan struct{}, msg control.Msg, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ch <- msg:
		return true
	case <-done:
		return false
	case <-timer.C:
		return false
	}
}

// ctrlSurface is control.Surface over a channel that lives as long as the
// surface does — the TUI's shape. The channel is created once and drained by
// the tview loop until the process exits, so it never goes away and the
// delivery bound is the only thing that can end a send.
type ctrlSurface struct {
	ch   chan<- control.Msg
	wait time.Duration // zero means tuiSendTimeout
}

// tuiSendTimeout bounds how long the TUI surface waits for the tview loop to
// take a prompt. The bridge's shared SendTimeout is sized for a surface that
// can legitimately be slow to attach (a browser stream still being set up);
// this one is drained continuously for the life of the process, so a channel
// that stays full is a wedged UI, and a prompt that cannot be shown is not
// worth parking the turn behind for thirty seconds. Five seconds is already
// more forgiving than the fail-fast send this replaced, and it keeps a stall
// from holding the agent for the whole bridge bound.
const tuiSendTimeout = 5 * time.Second

// Send implements control.Surface: the TUI's channel stays answerable for the
// life of the process, so it reports no teardown channel.
func (s ctrlSurface) Send(msg control.Msg) (<-chan struct{}, bool) {
	return nil, sendCtrlMsg(s.ch, nil, msg, s.bound())
}

func (s ctrlSurface) bound() time.Duration {
	if s.wait > 0 {
		return s.wait
	}
	return tuiSendTimeout
}

// installControlBridges wires the shared question and approval bridges onto
// sess for one surface. Every UI surface uses this one installer; only the
// transport differs.
//
// Both bridges are call-id aware: the approval bridge is the Session's
// SetApproveCallFn seam, so a decision travels with the provider's tool_call
// id all the way to the surface instead of being reduced to name+args.
func installControlBridges(sess *agentSession, surface control.Surface) {
	if reg := sess.ToolReg(); reg != nil {
		if qt, ok := reg.Get("question").(*tools.QuestionTool); ok {
			qt.Handler = func(ctx context.Context, entries []tools.QuestionEntry) []string {
				return control.AskQuestions(ctx, surface, entries)
			}
		}
		// The workflow_run tool runs synchronously on the agent's goroutine;
		// this bridge projects each committed lifecycle event onto the control
		// surface so the TUI can show per-node progress live instead of only
		// the report at completion (workflow-in-session Scope B).
		if wt, ok := reg.Get(toolcontract.ToolWorkflowRun).(*WorkflowRunTool); ok {
			wt.progress = func(p exec.ProgressEvent) {
				if msg := workflowProgressMsg(p); msg != nil {
					surface.Send(msg)
				}
			}
		}
	}
	// The run's ctx reaches the bridge so an aborted turn releases a parked
	// prompt instead of running out AnswerTimeout.
	sess.SetApproveCallFn(func(ctx context.Context, callID, name, args string) bool {
		return control.Approve(ctx, surface, callID, name, args)
	})
}

// workflowProgressMsg projects one exec progress fact onto the control-plane
// vocabulary. It returns nil for the scheduler's mechanical facts —
// settlement, fanout, loop, branch, aggregate, select, gate, and attempt
// bookkeeping — which a progress view does not render.
func workflowProgressMsg(p exec.ProgressEvent) *control.WorkflowProgress {
	m := &control.WorkflowProgress{NodePath: p.NodePath, Attempt: p.Attempt, Reason: p.Reason}
	switch p.Kind {
	case exec.EventRunStarted, exec.EventRunResumed:
		m.State = "started"
	case exec.EventNodeReady:
		m.State = "queued"
	case exec.EventDispatchStarted:
		m.State = "running"
	case exec.EventNodeSucceeded:
		m.State = "succeeded"
	case exec.EventNodeFailed:
		m.State = "failed"
	case exec.EventNodeSkipped:
		m.State = "skipped"
	case exec.EventNodeCancelled:
		m.State = "cancelled"
	case exec.EventRunFinished:
		m.State = "finished"
	case exec.EventBudgetExceeded:
		m.State = "finished"
		m.Reason = "budget exceeded"
	default:
		return nil
	}
	return m
}
