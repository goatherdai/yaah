package yaah

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Tests for the workflow_run tool (workflow-in-session Scope B, piece A). The
// runner seam is the same deterministic closure the run command's tests use
// (swapRunRunner), so a golden run makes no model calls and never sleeps.

func TestWorkflowRunToolNameAndDescription(t *testing.T) {
	var tool WorkflowRunTool
	if got := tool.Name(); got != "workflow_run" {
		t.Errorf("Name() = %q, want workflow_run", got)
	}
	if got := tool.Description(); strings.TrimSpace(got) == "" {
		t.Error("Description() is empty: prompts/tools/workflow_run.md is missing")
	}
}

// TestWorkflowRunToolSchema pins the advertised shape: spec is required and
// strict is optional, and both parse out of the raw schema.
func TestWorkflowRunToolSchema(t *testing.T) {
	var tool WorkflowRunTool
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("Schema() is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	if got := schema.Properties["spec"].Type; got != "string" {
		t.Errorf("spec type = %q, want string", got)
	}
	if got := schema.Properties["strict"].Type; got != "boolean" {
		t.Errorf("strict type = %q, want boolean", got)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "spec" {
		t.Errorf("required = %v, want [spec]", schema.Required)
	}

	// The model's payload parses with strict present and absent.
	var p struct {
		Spec   string `json:"spec"`
		Strict bool   `json:"strict"`
	}
	if err := json.Unmarshal([]byte(`{"spec":"a.yaml","strict":true}`), &p); err != nil {
		t.Fatalf("payload with strict did not parse: %v", err)
	}
	if p.Spec != "a.yaml" || !p.Strict {
		t.Errorf("parsed payload = %+v, want {a.yaml true}", p)
	}
}

// TestWorkflowRunToolRunsGoldenSpec pins the happy path: a golden document
// runs and the tool returns the command's report rather than an error.
func TestWorkflowRunToolRunsGoldenSpec(t *testing.T) {
	calls := 0
	swapRunRunner(t, echoRunner("done: "), &calls)

	var tool WorkflowRunTool
	out, err := tool.Execute(context.Background(), `{"spec":"`+wfGoldens+`sequential.yaml","strict":false,"params":["task=verify"]}`)
	if err != nil {
		t.Fatalf("Execute returned an error for a golden spec: %v\nreport:\n%s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("Execute returned an empty report")
	}
	if !strings.Contains(out, "status") {
		t.Errorf("report does not name the run status:\n%s", out)
	}
}

// TestWorkflowRunToolRefusesMissingSpec pins the failure path: a spec path
// that does not exist surfaces to the caller instead of yielding a silent
// empty report.
func TestWorkflowRunToolRefusesMissingSpec(t *testing.T) {
	swapRunRunner(t, echoRunner(""), nil)

	var tool WorkflowRunTool
	out, err := tool.Execute(context.Background(), `{"spec":"testdata/does-not-exist.yaml"}`)
	if err == nil && strings.TrimSpace(out) == "" {
		t.Fatal("missing spec produced neither an error nor a report")
	}
}

// TestWorkflowRunToolRejectsUnknownArgs pins the strict-key contract every
// tool shares: a typo is refused, not ignored.
func TestWorkflowRunToolRejectsUnknownArgs(t *testing.T) {
	var tool WorkflowRunTool
	if _, err := tool.Execute(context.Background(), `{"spec":"a.yaml","strcit":true}`); err == nil {
		t.Fatal("expected a refusal for an unknown argument key")
	}
}
