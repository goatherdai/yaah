package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestServer_workflowCapabilitiesReflectWiring pins the advertisement rule
// for the workflow selector surface: list is advertised only when its
// plumbing is wired, and the surface is absent otherwise.
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

	raw, err := json.Marshal(caps.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"list":{}`) {
		t.Errorf("workflowCapabilities wire missing list: %s", raw)
	}
	if strings.Contains(string(raw), `"run":`) {
		t.Errorf("workflowCapabilities must not advertise a run method: %s", raw)
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

