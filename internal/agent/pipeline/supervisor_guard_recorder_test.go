package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/types"
)

// TestSupervisorGuard_RecordsRoleAllowlistDeny pins the recorder seam:
// a refusal the dispatching role's own allowlist makes is handed to the
// recorder with the loop's owner, the tool, the command, and the reason
// the model was given — while an admitted call hands nothing over.
func TestSupervisorGuard_RecordsRoleAllowlistDeny(t *testing.T) {
	var recorded [][4]string
	rec := supervision.DenyRecorder(func(scopeID, tool, cmd, reason string) {
		recorded = append(recorded, [4]string{scopeID, tool, cmd, reason})
	})
	m := NewSupervisorGuardMiddleware(func(ownerID, toolName, argsJSON string) string { return "" },
		"sub-worker", []string{"go test*"}, rec)

	msg := &types.Message{ToolCalls: []types.ToolCall{
		toolCall("call-1", "bash", `{"command":"git status -s"}`),
		toolCall("call-2", "bash", `{"command":"go test ./..."}`),
	}}
	if _, err := m.PostModel(context.Background(), msg, &Step{}); err != nil {
		t.Fatalf("PostModel: %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("recorded = %v, want exactly the one refused call", recorded)
	}
	got := recorded[0]
	if got[0] != "sub-worker" || got[1] != "bash" || got[2] != "git status -s" {
		t.Errorf("recorded = %v, want the owner, tool, and refused command", got)
	}
	if !strings.Contains(got[3], "not in this role's allow_commands allowlist") {
		t.Errorf("recorded reason = %q, want the refusal the model was given", got[3])
	}
	// The deny still lands: recording never moves the verdict.
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "bash" {
		t.Errorf("remaining calls = %+v, want only the admitted shell call", msg.ToolCalls)
	}
}

// TestNewSubAgentPipeline_ThreadsDenyRecorder pins the config plumbing:
// a PipelineConfig carrying ShellGuardDenyRec installs a guard that
// records its role-allowlist refusals, so the wiring's recorder actually
// reaches the middleware rather than being dropped at the builder.
func TestNewSubAgentPipeline_ThreadsDenyRecorder(t *testing.T) {
	called := 0
	p := NewSubAgentPipeline(PipelineConfig{
		DestructiveGuard:  func(ownerID, toolName, argsJSON string) string { return "" },
		SessionID:         "sub-worker",
		ShellGuardAllow:   []string{"go test*"},
		ShellGuardDenyRec: supervision.DenyRecorder(func(scopeID, tool, cmd, reason string) { called++ }),
	})

	var guard Middleware
	for _, mw := range p.middleware {
		if mw.Name() == "supervisor_guard" {
			guard = mw
		}
	}
	if guard == nil {
		t.Fatal("supervisor_guard must install when the check is wired")
	}
	msg := &types.Message{ToolCalls: []types.ToolCall{
		toolCall("call-1", "bash", `{"command":"git status"}`),
	}}
	if _, err := guard.PostModel(context.Background(), msg, &Step{}); err != nil {
		t.Fatalf("PostModel: %v", err)
	}
	if called != 1 {
		t.Errorf("recorder calls = %d, want 1 — the config's recorder must reach the guard", called)
	}
}

// TestNewSubAgentPipeline_NilDenyRecorderRecordsNothing pins the
// backward-compatible shape: a config without a recorder denies exactly
// as before, with no recorder to hand the refusal to.
func TestNewSubAgentPipeline_NilDenyRecorderRecordsNothing(t *testing.T) {
	p := NewSubAgentPipeline(PipelineConfig{
		DestructiveGuard: func(ownerID, toolName, argsJSON string) string { return "" },
		SessionID:        "sub-worker",
		ShellGuardAllow:  []string{"go test*"},
	})

	var guard Middleware
	for _, mw := range p.middleware {
		if mw.Name() == "supervisor_guard" {
			guard = mw
		}
	}
	if guard == nil {
		t.Fatal("supervisor_guard must install when the check is wired")
	}
	msg := &types.Message{ToolCalls: []types.ToolCall{
		toolCall("call-1", "bash", `{"command":"git status"}`),
	}}
	step := &Step{}
	if _, err := guard.PostModel(context.Background(), msg, step); err != nil {
		t.Fatalf("PostModel: %v", err)
	}
	if len(step.SynthesizedResults) != 1 {
		t.Errorf("synthesized results = %d, want the denial synthesized unchanged", len(step.SynthesizedResults))
	}
}
