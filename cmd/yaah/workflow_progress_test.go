package yaah

import (
	"context"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/control"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/workflow/exec"
)

// Tests for the in-session workflow progress bridge (workflow-in-session
// Scope B): the projection onto the control plane, the installer wiring, and
// the tool's end-to-end delivery.

func TestWorkflowProgressMsg(t *testing.T) {
	cases := []struct {
		name string
		ev   exec.ProgressEvent
		want string // empty means the fact is filtered to nil
	}{
		{"run started", exec.ProgressEvent{Kind: exec.EventRunStarted}, "started"},
		{"node ready", exec.ProgressEvent{Kind: exec.EventNodeReady, NodePath: "a"}, "queued"},
		{"dispatch started", exec.ProgressEvent{Kind: exec.EventDispatchStarted, NodePath: "a", Attempt: 2}, "running"},
		{"node succeeded", exec.ProgressEvent{Kind: exec.EventNodeSucceeded, NodePath: "a"}, "succeeded"},
		{"node failed", exec.ProgressEvent{Kind: exec.EventNodeFailed, NodePath: "a", Reason: "boom"}, "failed"},
		{"node skipped", exec.ProgressEvent{Kind: exec.EventNodeSkipped, NodePath: "a", Reason: "guard"}, "skipped"},
		{"node cancelled", exec.ProgressEvent{Kind: exec.EventNodeCancelled, NodePath: "a", Reason: "halt"}, "cancelled"},
		{"run finished", exec.ProgressEvent{Kind: exec.EventRunFinished}, "finished"},
		{"attempt completed filtered", exec.ProgressEvent{Kind: exec.EventAttemptCompleted, NodePath: "a"}, ""},
		{"mechanical fact filtered", exec.ProgressEvent{Kind: exec.EventFanoutExpanded, NodePath: "a"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workflowProgressMsg(tc.ev)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("workflowProgressMsg(%s) = %+v, want nil", tc.ev.Kind, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("workflowProgressMsg(%s) = nil, want state %q", tc.ev.Kind, tc.want)
			}
			if got.State != tc.want {
				t.Errorf("workflowProgressMsg(%s).State = %q, want %q", tc.ev.Kind, got.State, tc.want)
			}
		})
	}
}

// TestInstallControlBridgesWorkflowRun pins that the installer reaches the
// workflow_run tool and that mechanical scheduler facts are filtered before
// they reach the surface.
func TestInstallControlBridgesWorkflowRun(t *testing.T) {
	ch := make(chan control.Msg, 8)
	reg := tools.NewRegistry()
	wt := &WorkflowRunTool{}
	reg.Register(wt)
	sess := &agentSession{toolReg: reg}
	installControlBridges(sess, ctrlSurface{ch: ch})

	if wt.progress == nil {
		t.Fatal("workflow_run progress bridge missing")
	}
	wt.progress(exec.ProgressEvent{Kind: exec.EventNodeSucceeded, NodePath: "steps.a"})
	wt.progress(exec.ProgressEvent{Kind: exec.EventFanoutExpanded, NodePath: "steps.a"})

	var got []*control.WorkflowProgress
	deadline := time.After(2 * time.Second)
	for len(got) == 0 {
		select {
		case m := <-ch:
			wp, ok := m.(*control.WorkflowProgress)
			if !ok {
				t.Fatalf("surface received %T, want *control.WorkflowProgress", m)
			}
			got = append(got, wp)
		case <-deadline:
			t.Fatal("progress message never reached the surface")
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d workflow progress messages, want 1", len(got))
	}
	if got[0].State != "succeeded" || got[0].NodePath != "steps.a" {
		t.Errorf("got %+v, want succeeded for steps.a", got[0])
	}
}

// TestWorkflowRunToolReportsProgress pins the full seam end-to-end: the tool's
// progress callback receives run_started, the delegate node's lifecycle, and
// run_finished, driven through the real host door on the golden sequential
// spec with the deterministic echo runner.
func TestWorkflowRunToolReportsProgress(t *testing.T) {
	swapRunRunner(t, echoRunner("done: "), nil)

	var tool WorkflowRunTool
	var got []exec.ProgressEvent
	tool.progress = func(p exec.ProgressEvent) { got = append(got, p) }

	_, err := tool.Execute(context.Background(), `{"spec":"`+wfGoldens+`sequential.yaml","strict":false,"params":["task=verify"]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	counts := map[string]int{}
	for _, p := range got {
		counts[string(p.Kind)+"/"+p.NodePath]++
	}
	if counts[string(exec.EventRunStarted)+"/"] != 1 {
		t.Errorf("run_started seen %d times, want 1", counts[string(exec.EventRunStarted)+"/"])
	}
	if counts[string(exec.EventRunFinished)+"/"] != 1 {
		t.Errorf("run_finished seen %d times, want 1", counts[string(exec.EventRunFinished)+"/"])
	}
	for _, kind := range []exec.EventKind{exec.EventNodeReady, exec.EventDispatchStarted, exec.EventNodeSucceeded} {
		if counts[string(kind)+"/steps.plan"] != 1 {
			t.Errorf("%s for steps.plan seen %d times, want 1", kind, counts[string(kind)+"/steps.plan"])
		}
	}
}
