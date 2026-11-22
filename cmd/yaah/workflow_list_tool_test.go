package yaah

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the workflow_list tool (workflow default-location feature).

func TestWorkflowListToolNameAndDescription(t *testing.T) {
	var tool WorkflowListTool
	if got := tool.Name(); got != "workflow_list" {
		t.Errorf("Name() = %q, want workflow_list", got)
	}
	if got := tool.Description(); strings.TrimSpace(got) == "" {
		t.Error("Description() is empty: prompts/tools/workflow_list.md is missing")
	}
}

func TestWorkflowListToolLists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeListSpec(t, filepath.Join(cwd, "workflows", "foo.yaml"), "foo")
	writeListSpec(t, filepath.Join(home, "workflows", "bar.yaml"), "bar")

	var tool WorkflowListTool
	out, err := tool.Execute(context.Background(), "{}")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, name := range []string{"foo", "bar"} {
		if !strings.Contains(out, name) {
			t.Fatalf("output missing %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, filepath.Join(cwd, "workflows", "foo.yaml")) {
		t.Errorf("foo resolved to the wrong path:\n%s", out)
	}
}

func TestWorkflowListToolEmpty(t *testing.T) {
	t.Setenv("YAAH_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	var tool WorkflowListTool
	out, err := tool.Execute(context.Background(), "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "no workflow specs found") {
		t.Errorf("empty listing = %q, want the explicit empty report", out)
	}
}

func TestWorkflowListToolRejectsUnknownArgs(t *testing.T) {
	var tool WorkflowListTool
	if _, err := tool.Execute(context.Background(), `{"foo":1}`); err == nil {
		t.Fatal("expected a refusal for an unknown argument key")
	}
}
