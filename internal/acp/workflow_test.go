package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestServer_workflowCapabilitiesReflectWiring pins the advertisement rule
// for the workflow selector surface: list/run are advertised only when
// their plumbing is wired.
func TestServer_workflowCapabilitiesReflectWiring(t *testing.T) {
	s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
	if s.capabilities().Workflow != nil {
		t.Fatal("workflowCapabilities advertised without callbacks")
	}
	s.ListWorkflows = func(string) ([]WorkflowInfo, error) { return nil, nil }
	caps := s.capabilities()
	if caps.Workflow == nil || caps.Workflow.List == nil {
		t.Fatal("workflow/list must be advertised once ListWorkflows is wired")
	}
	if caps.Workflow.Run != nil {
		t.Fatal("workflow/run advertised without a RunWorkflow callback")
	}
	s.RunWorkflow = func(string, string) (WorkflowRunResult, error) { return WorkflowRunResult{}, nil }
	caps = s.capabilities()
	if caps.Workflow.Run == nil {
		t.Fatal("workflow/run must be advertised once RunWorkflow is wired")
	}

	raw, err := json.Marshal(caps.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"list":{}`, `"run":{}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("workflowCapabilities wire missing %s: %s", want, raw)
		}
	}
}

// TestServer_handleWorkflowList pins the workflow/list contract: specs are
// surfaced, an empty page is an empty array, a malformed cwd is invalid
// params, and callback errors are internal errors.
func TestServer_handleWorkflowList(t *testing.T) {
	t.Run("empty page is an empty array", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.ListWorkflows = func(string) ([]WorkflowInfo, error) { return nil, nil }
		msg := Message{ID: json.RawMessage("1")}
		var result json.RawMessage
		var errCode int
		var errMsg string
		s.handleWorkflowList(msg,
			func(_ json.RawMessage, r any) { result, _ = json.Marshal(r) },
			func(_ json.RawMessage, c int, m string) { errCode, errMsg = c, m },
		)
		if errCode != 0 {
			t.Fatalf("unexpected error %d: %s", errCode, errMsg)
		}
		if string(result) != `{"workflows":[]}` {
			t.Fatalf("empty page not an empty array: %s", result)
		}
	})

	t.Run("specs surface in order", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.ListWorkflows = func(cwd string) ([]WorkflowInfo, error) {
			if cwd == "" {
				return []WorkflowInfo{{Name: "b", Path: "/g/b.yaml"}}, nil
			}
			return []WorkflowInfo{{Name: "a", Path: "/w/a.yaml"}}, nil
		}
		msg := Message{ID: json.RawMessage("1"), Params: json.RawMessage(`{"cwd":"/w"}`)}
		var result json.RawMessage
		s.handleWorkflowList(msg,
			func(_ json.RawMessage, r any) { result, _ = json.Marshal(r) },
			func(_ json.RawMessage, _ int, _ string) {},
		)
		if !strings.Contains(string(result), `"name":"a"`) {
			t.Fatalf("workflow/list result missing wired spec: %s", result)
		}
	})

	t.Run("malformed cwd is invalid params", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.ListWorkflows = func(string) ([]WorkflowInfo, error) { return nil, nil }
		msg := Message{ID: json.RawMessage("1"), Params: json.RawMessage(`{"cwd":"relative"}`)}
		var errCode int
		s.handleWorkflowList(msg,
			func(_ json.RawMessage, _ any) {},
			func(_ json.RawMessage, c int, _ string) { errCode = c },
		)
		if errCode != codeInvalidParams {
			t.Fatalf("relative cwd not invalid params: got %d", errCode)
		}
	})

	t.Run("callback error is internal", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.ListWorkflows = func(string) ([]WorkflowInfo, error) { return nil, fmt.Errorf("boom") }
		msg := Message{ID: json.RawMessage("1")}
		var errCode int
		s.handleWorkflowList(msg,
			func(_ json.RawMessage, _ any) {},
			func(_ json.RawMessage, c int, _ string) { errCode = c },
		)
		if errCode != codeInternal {
			t.Fatalf("callback error not internal: got %d", errCode)
		}
	})
}

// TestServer_handleWorkflowRun pins the workflow/run contract: a missing
// spec is invalid params, and the run result is delivered (the callback
// runs off the dispatch loop, so the response arrives via respOK).
func TestServer_handleWorkflowRun(t *testing.T) {
	t.Run("missing spec is invalid params", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.RunWorkflow = func(string, string) (WorkflowRunResult, error) {
			t.Fatal("RunWorkflow called for a missing spec")
			return WorkflowRunResult{}, nil
		}
		msg := Message{ID: json.RawMessage("1")}
		var errCode int
		s.handleWorkflowRun(msg, nil,
			func(_ json.RawMessage, _ any) {},
			func(_ json.RawMessage, c int, _ string) { errCode = c },
		)
		if errCode != codeInvalidParams {
			t.Fatalf("missing spec not invalid params: got %d", errCode)
		}
	})

	t.Run("run result delivered asynchronously", func(t *testing.T) {
		s := NewServer(func(BuildOptions) (Session, error) { return nil, fmt.Errorf("not used") }, "test", nil, nil)
		s.RunWorkflow = func(cwd, spec string) (WorkflowRunResult, error) {
			if cwd != "/w" || spec != "deploy" {
				return WorkflowRunResult{}, fmt.Errorf("unexpected args %q %q", cwd, spec)
			}
			return WorkflowRunResult{OK: true, Report: "done"}, nil
		}
		msg := Message{ID: json.RawMessage("1"), Params: json.RawMessage(`{"cwd":"/w","spec":"deploy"}`)}
		got := make(chan WorkflowRunResult, 1)
		errs := make(chan string, 1)
		s.handleWorkflowRun(msg, nil,
			func(_ json.RawMessage, r any) { got <- r.(WorkflowRunResult) },
			func(_ json.RawMessage, _ int, m string) { errs <- m },
		)
		select {
		case r := <-got:
			if !r.OK || r.Report != "done" {
				t.Fatalf("unexpected run result: %+v", r)
			}
		case m := <-errs:
			t.Fatalf("run error: %s", m)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for workflow/run result")
		}
	})
}
