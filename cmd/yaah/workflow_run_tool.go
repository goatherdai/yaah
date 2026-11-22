package yaah

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/workflow/discover"
	"github.com/goatherdai/yaah/internal/workflow/exec"
	"github.com/spf13/cobra"
)

// workflow_run_tool.go puts `yaah workflow run` on the orchestrator's belt as
// a tool (workflow-in-session Scope B, piece A).
//
// Layering, and why this lives in cmd/yaah and not internal/tools: the
// workflow host package imports internal/tools, so a tool there that reached
// the host (or the run command) would close an import cycle. This package
// already imports both, so the tool is a thin adapter over the same
// runWorkflowRun the CLI calls — one implementation of the command, two
// surfaces.
//
// The tool is host-side like the command: it validates the document with the
// same pipeline `validate` uses, and a document that fails is refused with
// its diagnostics rather than dispatched on. The report the tool returns is
// the command's own textual report — the orchestrator reads the same status,
// reason and outputs a human would.
// WorkflowRunTool exposes `yaah workflow run` to the orchestrator as a tool
// (workflow-in-session Scope B, piece A). It is a thin adapter over
// runWorkflowRun: the run is synchronous, and the tool's result is the run
// report the command would have printed.
type WorkflowRunTool struct {
	// progress, when non-nil, receives each committed external lifecycle
	// event's projection as the run executes. installControlBridges wires it
	// to the control surface for the TUI; it stays nil in a headless session,
	// where there is no view to feed and the report at completion is enough.
	progress func(exec.ProgressEvent)

	WS tools.Workspace
}

var _ tools.WorkspaceSetter = (*WorkflowRunTool)(nil)

// SetWorkspace receives the session's workspace (injected by the registry),
// so a bare spec name resolves against the session's working directory rather
// than the server process's.
func (t *WorkflowRunTool) SetWorkspace(ws tools.Workspace) { t.WS = ws }

func (t *WorkflowRunTool) Name() string { return "workflow_run" }

func (t *WorkflowRunTool) Description() string {
	return prompts.ToolDescription("workflow_run")
}

func (t *WorkflowRunTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "spec": {
      "type": "string",
      "description": "Path to the workflow document to run (.yaml or .json), or a bare name resolved against the search directories (./workflows then ~/.yaah/workflows). Validated and resolved exactly as yaah workflow validate would."
    },
    "strict": {
      "type": "boolean",
      "description": "Treat validation warnings as refusals, like --strict on the command line. Default false."
    },
    "params": {
      "type": "array",
      "items": {"type": "string"},
      "description": "key=value parameter bindings for the document, like --param task=verify on the CLI."
    }
  },
  "required": ["spec"]
}`)
}

func (t *WorkflowRunTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct {
		Spec   string   `json:"spec"`
		Strict bool     `json:"strict"`
		Params []string `json:"params"`
	}
	if err := tools.StrictToolArgs("workflow_run", t.Schema(), args, &p); err != nil {
		return "", err
	}
	if p.Spec == "" {
		return "", fmt.Errorf("workflow_run: spec is required — the path to a workflow document")
	}
	// Resolve a bare name against the session's workspace, then hand an
	// absolute path to runWorkflowRun (which otherwise re-resolves against
	// the process cwd and would pick the wrong directory for a session with
	// its own working directory).
	if resolved, rerr := discover.Resolve(toolWorkDir(t.WS), p.Spec); rerr != nil {
		return "", rerr
	} else {
		p.Spec = resolved
	}
	// Same single-flight gate as the TUI and ACP paths: the runner exchanges
	// run-specific retained runtime through a process-global pointer, so
	// overlapping runs would cross wires.
	if !workflowRunGate.TryLock() {
		return "", fmt.Errorf("workflow_run: a workflow run is already in progress")
	}
	defer workflowRunGate.Unlock()

	// The command writes its report to the writer it is handed and returns an
	// error for the two failure classes the CLI distinguishes: a document
	// refused before dispatch (whose diagnostics are the report) and a run
	// that reached the door and stopped there. Both must come back to the
	// model as the report text, so the buffer is preferred over the error
	// whenever it holds anything; the error is returned only when the buffer
	// is empty and there is otherwise nothing to show.
	var buf bytes.Buffer
	c := &cobra.Command{Use: "workflow run"}
	c.SetOut(&buf)
	c.SetErr(&buf)
	if ctx != nil {
		c.SetContext(ctx)
	}
	err := runWorkflowRun(c, p.Spec, workflowRunOptions{
		workflowOptions: workflowOptions{strict: p.Strict, params: p.Params},
		progress:        t.progress,
	})
	if err != nil && buf.Len() == 0 {
		return "", err
	}
	return buf.String(), nil
}
