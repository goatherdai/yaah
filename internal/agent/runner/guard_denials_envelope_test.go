package runner

// guard_denials_envelope_test.go proves the whole chain end to end: a real
// sub-agent loop whose guard refuses a call, dispatched through the real
// dispatch lane, and read back from the model-facing envelope's warnings.
// The runner-level tests prove each deny path reaches the sink and the
// dispatch-level tests prove the sink reaches the envelope; this joins the two
// so neither half can drift without failing.

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/dispatch"
)

// dispatchThroughSharedLane runs one dispatch of opts through the dispatch
// core's shared lane and returns the envelope the orchestrator would read.
func dispatchThroughSharedLane(t *testing.T, opts taskRunnerOpts, prompt, role string) dispatch.ProposalEnvelope {
	t.Helper()
	res, err := dispatch.Run(context.Background(), dispatch.DispatchRequest{
		Lane:   dispatch.LaneShared,
		Params: dispatch.SubAgentParams{Role: role},
		Prompt: prompt,
		Runner: makeTaskRunner(opts, 0),
	})
	if err != nil {
		t.Fatalf("dispatch.Run: %v", err)
	}
	return res.Envelope()
}

// TestGuardDenialsReachTheDispatchEnvelope is the acceptance test for yaah-938:
// both deny paths — the global guard's pre-dispatch check and the dispatching
// role's own allow_commands allowlist — must land in the envelope's warnings,
// because the orchestrator reads the envelope and never the trace.
func TestGuardDenialsReachTheDispatchEnvelope(t *testing.T) {
	initTestRoles(t)

	t.Run("global guard denial", func(t *testing.T) {
		const reason = "Blocked destructive operation. Supervisor guard denied."
		env := dispatchThroughSharedLane(t, taskRunnerOpts{
			provider:     &toolCallProvider{cmd: "rm -rf src"},
			systemPrompt: "BASE",
			modelName:    "test-model",
			subCfg:       config.SubAgentConfig{},
			defaults:     config.Defaults{},
			guardCheck: func(ownerID, toolName, argsJSON string) string {
				if toolName == "bash" {
					return reason
				}
				return ""
			},
		}, "clean the tree", "analyst")

		joined := strings.Join(env.Warnings, "\n")
		want := "supervisor guard denied bash (rm -rf src): " + reason
		if !strings.Contains(joined, want) {
			t.Fatalf("envelope warnings = %q, want %q", env.Warnings, want)
		}
		if !strings.Contains(env.Warning, "supervisor guard denied bash") {
			t.Errorf("envelope warning = %q, want the refusal in the scalar field too", env.Warning)
		}
	})

	t.Run("role allowlist denial", func(t *testing.T) {
		env := dispatchThroughSharedLane(t, taskRunnerOpts{
			provider:     &toolCallProvider{cmd: "git status"},
			systemPrompt: "BASE",
			modelName:    "test-model",
			subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
				"developer": {AllowCommands: []string{"go test*"}},
			}},
			defaults: config.Defaults{},
			// The global guard admits everything, so only the role's own
			// allowlist can refuse this call — the path a check-only capture
			// would miss.
			guardCheck: func(ownerID, toolName, argsJSON string) string { return "" },
		}, "look at the tree", "developer")

		joined := strings.Join(env.Warnings, "\n")
		if !strings.Contains(joined, "supervisor guard denied bash (git status) by role_shell_allowlist") {
			t.Fatalf("envelope warnings = %q, want the allowlist refusal naming its rule", env.Warnings)
		}
		if !strings.Contains(joined, "allow_commands allowlist") {
			t.Errorf("envelope warnings = %q, want the refusal reason the model was given", env.Warnings)
		}
	})
}
