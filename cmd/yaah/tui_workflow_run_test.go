package yaah

import (
	"strings"
	"testing"
)

// Tests for the `yaah tui workflow run <spec>` launcher (workflow-in-session
// Scope B, piece C).

func TestWorkflowSeedPrompt(t *testing.T) {
	got := workflowSeedPrompt("/tmp/wf.yaml")
	if !strings.Contains(got, `"/tmp/wf.yaml"`) {
		t.Errorf("seed prompt does not name the spec path: %q", got)
	}
	if !strings.Contains(got, "workflow_run") {
		t.Errorf("seed prompt does not name the workflow_run tool: %q", got)
	}
}

// TestTUIWorkflowRunSubcommand pins the launcher's shape: it lives under
// `tui workflow run`, takes exactly one spec argument, and no flag has slipped
// onto the leaf that would shadow the refusal or the spec position.
func TestTUIWorkflowRunSubcommand(t *testing.T) {
	cmd, _, err := tuiCmd.Find([]string{"workflow", "run"})
	if err != nil {
		t.Fatalf("Find(workflow run): %v", err)
	}
	if cmd != tuiWorkflowRunCmd {
		t.Fatalf("Find(workflow run) = %v, want tuiWorkflowRunCmd", cmd)
	}
	if cmd.Use != "run <spec>" {
		t.Errorf("Use = %q, want %q", cmd.Use, "run <spec>")
	}
	if err := cmd.Args(cmd, nil); err == nil {
		t.Error("Args() accepted zero args, want exactly one spec arg")
	}
	if err := cmd.Args(cmd, []string{"a.yaml", "b.yaml"}); err == nil {
		t.Error("Args() accepted two args, want exactly one spec arg")
	}
	if err := cmd.Args(cmd, []string{"a.yaml"}); err != nil {
		t.Errorf("Args(one) = %v, want nil", err)
	}
}
