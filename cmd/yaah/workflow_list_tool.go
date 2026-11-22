package yaah

// workflow_list_tool.go exposes `yaah workflow list` to the orchestrator
// (workflow default-location feature): the agent discovers the runnable specs
// (name -> path) before choosing one for workflow_run.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/workflow/discover"
)

// toolWorkDir returns the session's working directory when the tool was
// injected with a workspace, falling back to the process cwd otherwise
// (standalone/tests). The fallback keeps the previous behaviour for tools
// never mounted on a session.
func toolWorkDir(ws tools.Workspace) string {
	if ws != nil && ws.WorkDir() != "" {
		return ws.WorkDir()
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// WorkflowListTool lists the workflow documents discoverable from the search
// directories (./workflows then ~/.yaah/workflows). It is the discovery
// companion to workflow_run: the agent lists, then runs a name from the list.
type WorkflowListTool struct {
	WS tools.Workspace
}

var _ tools.WorkspaceSetter = (*WorkflowListTool)(nil)

// SetWorkspace receives the session's workspace (injected by the registry),
// so the tool lists the workflows of the session's working directory rather
// than the server process's.
func (t *WorkflowListTool) SetWorkspace(ws tools.Workspace) { t.WS = ws }

func (t *WorkflowListTool) Name() string { return "workflow_list" }

func (t *WorkflowListTool) Description() string {
	return prompts.ToolDescription("workflow_list")
}

func (t *WorkflowListTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {}
}`)
}

func (t *WorkflowListTool) Execute(ctx context.Context, args string) (string, error) {
	var p struct{}
	if err := tools.StrictToolArgs("workflow_list", t.Schema(), args, &p); err != nil {
		return "", err
	}
	cwd := toolWorkDir(t.WS)
	specs, err := discover.List(cwd)
	if err != nil {
		return "", fmt.Errorf("workflow_list: %w", err)
	}
	if len(specs) == 0 {
		return "no workflow specs found", nil
	}
	var b strings.Builder
	for _, s := range specs {
		fmt.Fprintf(&b, "%s\t%s\n", s.Name, s.Path)
	}
	return b.String(), nil
}
