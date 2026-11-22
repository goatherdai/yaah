package pipeline

import (
	"context"
	"fmt"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/types"
)

// SupervisorGuardMiddleware is the blocking pre-dispatch net for
// sub-agent loops (goatherd-adoption plan B2, decision D3): sub-agent
// loops run at ApprovalMode "allow" — the parent's approval gate
// never sees their calls — so a destructive call dispatched yesterday
// with no gate at all. The middleware consults the supervision
// service's synchronous check BEFORE execution and synthesizes a
// denied-call result in the same shape the permission and approval
// middlewares produce, so the model sees a response for every call
// and can take the structured path instead.
//
// Deny-first is the contract: the check returns a reason exactly when
// the call must not dispatch. The structured tools (write, edit,
// delete) are the approved path for changes — their review is the
// merge gate's job (Phase C), not this net; a shell call is denied
// unless the dispatching role's allow_commands admits its command
// (empty allowlist = deny every shell call, the historical default).
type SupervisorGuardMiddleware struct {
	// check denies a call by returning a non-empty reason. Nil means
	// the guard is disabled and every call passes.
	check func(ownerID, toolName, argsJSON string) string
	// ownerID is the sub-agent's trace owner, the scope the check
	// addresses and any rule state keys on.
	ownerID string
}

// NewSupervisorGuardMiddleware builds the guard for one sub-agent loop.
// allow is the dispatching role's resolved shell-command allowlist; nil
// inherits the global default the check already carries, and an
// explicitly empty list denies every shell call (see
// supervision.WithShellAllowlist). rec, when wired, records each
// role-allowlist refusal so it stays legible on the supervision
// status and trace surfaces; omitting it records nothing.
func NewSupervisorGuardMiddleware(check func(ownerID, toolName, argsJSON string) string, ownerID string, allow []string, rec ...supervision.DenyRecorder) *SupervisorGuardMiddleware {
	return &SupervisorGuardMiddleware{check: supervision.WithShellAllowlist(check, allow, rec...), ownerID: ownerID}
}

func (m *SupervisorGuardMiddleware) Name() string { return "supervisor_guard" }

func (m *SupervisorGuardMiddleware) PrepareStep(ctx context.Context, step *Step) (*Step, error) {
	return step, nil
}

func (m *SupervisorGuardMiddleware) PostModel(ctx context.Context, msg *types.Message, step *Step) (*Step, error) {
	if m.check == nil || len(msg.ToolCalls) == 0 {
		return step, nil
	}

	filtered := make([]types.ToolCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		reason := m.check(m.ownerID, tc.Function.Name, tc.Function.Arguments)
		if reason == "" {
			filtered = append(filtered, tc)
			continue
		}
		// Every tool_call_id on the assistant message needs a result;
		// synthesize the denial in the approval middleware's own
		// denied-call shape so the provider does not reject the next
		// request over a dangling ID — and the model reads the same
		// error surface it already knows how to respond to.
		step.SynthesizedResults = append(step.SynthesizedResults,
			types.ToolResultMsg(tc.ID, tc.Function.Name,
				fmt.Sprintf("error: tool %q denied by supervisor guard: %s", tc.Function.Name, reason)))
	}
	msg.ToolCalls = filtered
	return step, nil
}

func (m *SupervisorGuardMiddleware) PostTool(ctx context.Context, results []ToolResult, step *Step) (*Step, error) {
	return step, nil
}
