package runner

// guard_denials.go records the supervisor-guard refusals of ONE sub-agent run
// so the dispatch envelope can report them (yaah-938). The trace half of the
// same refusal — the guard's deny facts and the supervision status firings —
// is already recorded by the service; this is the model-facing half, because an
// orchestrator reading only the child's report would take a task as finished
// when part of what the child attempted was refused.

import (
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/tools"
)

// denialLog accumulates one run's refusals. It is written on the loop's own
// goroutine — the guard middleware runs pre-dispatch, before the tools it
// admits fan out — and read once, after the loop returns.
type denialLog struct {
	denials []tools.SubAgentDenial
}

// newDenialLog returns an empty log for one dispatch.
func newDenialLog() *denialLog { return &denialLog{} }

// wrap returns this run's guard check and role-allowlist deny recorder, both
// of which record their refusals here before returning the caller's verdict
// unchanged, together with the refusals already recorded.
//
// The wrapping is per run and never at the service: the check and the recorder
// are created once at wiring and shared by the parent loop and by sibling
// dispatches, so wrapping them there would attribute another run's refusals to
// this child.
//
// Both paths must be wrapped because they are disjoint: the role allowlist is
// consulted BEFORE the wrapped global check (supervision.WithShellAllowlistPlan
// refuses without calling it), and the global guard refuses calls the allowlist
// never classified. A refusal from either path is invisible to the other.
//
// A nil check means the guard is off for this session, in which case the
// pipeline installs no guard middleware at all and the allowlist never runs
// either: the recorder is returned untouched.
func (l *denialLog) wrap(check func(ownerID, toolName, argsJSON string) string, rec supervision.DenyRecorder) (func(ownerID, toolName, argsJSON string) string, supervision.DenyRecorder) {
	if check == nil {
		return nil, rec
	}
	guardCheck := func(ownerID, toolName, argsJSON string) string {
		reason := check(ownerID, toolName, argsJSON)
		if reason != "" {
			l.add(tools.SubAgentDenial{
				Tool:    toolName,
				Command: supervision.ToolCommand(argsJSON),
				Reason:  reason,
			})
		}
		return reason
	}
	// The recorder is installed even when the wiring supplied none: capturing
	// the refusal is this run's own bookkeeping and never moves the verdict,
	// so a session without a supervision service still reports what it refused.
	denyRec := func(scopeID, tool, cmd, reason string) {
		if rec != nil {
			rec(scopeID, tool, cmd, reason)
		}
		l.add(tools.SubAgentDenial{
			Tool:    tool,
			Command: cmd,
			Reason:  reason,
			Rule:    supervision.ShellAllowlistRule,
		})
	}
	return guardCheck, denyRec
}

// add records one refusal, dropping an exact repeat. A model that keeps
// retrying a refused command records once, so the envelope names the distinct
// refusals instead of one line per turn.
func (l *denialLog) add(d tools.SubAgentDenial) {
	if d.Tool == "" && d.Reason == "" {
		return
	}
	for _, seen := range l.denials {
		if seen == d {
			return
		}
	}
	l.denials = append(l.denials, d)
}

// list returns the refusals recorded so far, for the dispatch sink.
func (l *denialLog) list() []tools.SubAgentDenial { return l.denials }
