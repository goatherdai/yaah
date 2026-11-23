package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// toolCallProvider answers the first model call with one bash tool call and
// every later call with a plain report — the shape a guarded sub-agent has when
// the guard refuses the only call it tried to make.
type toolCallProvider struct {
	calls int
	cmd   string
}

func (p *toolCallProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &types.ChatResponse{
			Choices: []types.Choice{{
				Message: types.Message{Role: "assistant", ToolCalls: []types.ToolCall{{
					ID:       "call-1",
					Type:     "function",
					Function: types.ToolCallFn{Name: "bash", Arguments: `{"command":"` + p.cmd + `"}`},
				}}},
				FinishReason: "tool_calls",
			}},
			Usage: types.Usage{TotalTokens: 5},
		}, nil
	}
	return &types.ChatResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "the guarded command was refused; reporting without it"},
			FinishReason: "stop",
		}},
		Usage: types.Usage{TotalTokens: 5},
	}, nil
}

// TestTaskRunner_CapturesGlobalGuardDenial pins the global destructive-guard
// deny path (yaah-938): a call the injected check refuses is recorded for the
// dispatch envelope, with the tool the command and the reason the model got.
func TestTaskRunner_CapturesGlobalGuardDenial(t *testing.T) {
	initTestRoles(t)
	const reason = "Blocked destructive operation. Supervisor guard denied."
	var checked int
	opts := taskRunnerOpts{
		provider:     &toolCallProvider{cmd: "rm -rf src"},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		guardCheck: func(ownerID, toolName, argsJSON string) string {
			checked++
			if toolName == "bash" {
				return reason
			}
			return ""
		},
	}

	var got []tools.SubAgentDenial
	ctx := tools.WithSubAgentDenials(context.Background(), &got)
	result, err := makeTaskRunner(opts, 0)(ctx, "clean the tree", tools.SubAgentParams{Role: "analyst"})
	if err != nil {
		t.Fatalf("run: %v (result %q)", err, result)
	}
	if checked != 1 {
		t.Fatalf("guard consulted %d times, want the one refused call", checked)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d denials, want 1: %+v", len(got), got)
	}
	d := got[0]
	if d.Tool != "bash" || d.Command != "rm -rf src" {
		t.Errorf("captured %+v, want the refused bash call and its command", d)
	}
	if d.Reason != reason {
		t.Errorf("reason = %q, want the refusal the model was given %q", d.Reason, reason)
	}
	if d.Rule != "" {
		t.Errorf("rule = %q, want empty on the global guard's path", d.Rule)
	}
}

// TestTaskRunner_CapturesAllowlistDenial pins the second, disjoint deny path
// (yaah-938): the dispatching role's own allow_commands list refuses the call
// BEFORE the wrapped global check runs, so a capture that only wrapped the
// check would miss it. The refusal is reported with the allowlist rule name.
func TestTaskRunner_CapturesAllowlistDenial(t *testing.T) {
	initTestRoles(t)
	opts := taskRunnerOpts{
		provider:     &toolCallProvider{cmd: "git status"},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {AllowCommands: []string{"go test*"}},
		}},
		defaults: config.Defaults{},
		// The global guard admits everything: only the role's allowlist can
		// refuse this call.
		guardCheck: func(ownerID, toolName, argsJSON string) string { return "" },
	}

	var got []tools.SubAgentDenial
	ctx := tools.WithSubAgentDenials(context.Background(), &got)
	if _, err := makeTaskRunner(opts, 0)(ctx, "look at the tree", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d denials, want 1: %+v", len(got), got)
	}
	d := got[0]
	if d.Tool != "bash" || d.Command != "git status" {
		t.Errorf("captured %+v, want the refused bash call and its command", d)
	}
	if d.Rule != supervision.ShellAllowlistRule {
		t.Errorf("rule = %q, want %q", d.Rule, supervision.ShellAllowlistRule)
	}
	if !strings.Contains(d.Reason, "allow_commands allowlist") {
		t.Errorf("reason = %q, want the allowlist refusal the model was given", d.Reason)
	}
}

// TestDenialLog_DropsExactRepeats pins the bound: a model that retries the same
// refused command every turn records it once, so the envelope names distinct
// refusals instead of one line per turn.
func TestDenialLog_DropsExactRepeats(t *testing.T) {
	log := newDenialLog()
	check, _ := log.wrap(
		func(ownerID, toolName, argsJSON string) string { return "denied" },
		nil,
	)
	const args = `{"command":"rm -rf src"}`
	if got := check("sub-worker", "bash", args); got != "denied" {
		t.Fatalf("verdict = %q, want the wrapped check's verdict unchanged", got)
	}
	check("sub-worker", "bash", args)
	if n := len(log.list()); n != 1 {
		t.Fatalf("recorded %d denials for two identical refusals, want 1", n)
	}
}

// TestDenialLog_NilCheckLeavesRecorderUntouched pins the guard-off shape: with
// no global check the pipeline installs no guard middleware, so the recorder is
// passed through unchanged rather than becoming a second deny authority.
func TestDenialLog_NilCheckLeavesRecorderUntouched(t *testing.T) {
	log := newDenialLog()
	rec := supervision.DenyRecorder(func(scopeID, tool, cmd, reason string) {})
	check, got := log.wrap(nil, rec)
	if check != nil {
		t.Error("a nil guard check must stay nil")
	}
	if got == nil {
		t.Error("the recorder must come back untouched")
	}
	if n := len(log.list()); n != 0 {
		t.Fatalf("recorded %d denials with no guard, want 0", n)
	}
}
