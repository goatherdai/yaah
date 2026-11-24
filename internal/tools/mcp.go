package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/goatherdai/yaah/internal/prompts"
)

// MCPServerStatus is the neutral per-server report the mcp tool renders.
// The wiring layer adapts live MCP clients into it, so this package never
// imports internal/mcp.
type MCPServerStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	URL       string   `json:"url,omitempty"`
	Connected bool     `json:"connected"`
	ToolCount int      `json:"tool_count"` // derived from Tools on render
	Tools     []string `json:"tools"`
	Error     string   `json:"error,omitempty"`
}

// MCPServerLister reports the session's MCP servers and the tools each
// exposes. Injected at wiring time in the tools.NewRegistry doc pattern
// (Register(&MyWiredTool{...})).
type MCPServerLister func() []MCPServerStatus

// MCPListTool reports the MCP servers connected to the session and the
// tools each exposes (bead yaah-skzs). `yaah mcp list` sees only the
// configuration; the tool surface otherwise gives a model no way to ask
// what is actually connected. Read-only.
type MCPListTool struct {
	Lister MCPServerLister
}

// Name returns the tool name.
func (t *MCPListTool) Name() string { return "mcp" }

// Description returns the prompt description from prompts/tools/mcp.md.
func (t *MCPListTool) Description() string {
	return prompts.ToolDescription("mcp")
}

// Schema returns the tool's input schema: no parameters.
func (t *MCPListTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// Execute renders the inventory as pretty-printed JSON. Ordering is
// deterministic regardless of provider order: servers sorted by name,
// tool names sorted, tool_count derived from the names shown.
func (t *MCPListTool) Execute(ctx context.Context, args string) (string, error) {
	var noArgs struct{}
	if err := StrictToolArgs(t.Name(), t.Schema(), args, &noArgs); err != nil {
		return "", err
	}
	if t.Lister == nil {
		return "", fmt.Errorf("mcp: no server lister wired")
	}
	servers := t.Lister()
	if servers == nil {
		servers = []MCPServerStatus{}
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	for i := range servers {
		if servers[i].Tools == nil {
			servers[i].Tools = []string{}
		}
		sort.Strings(servers[i].Tools)
		servers[i].ToolCount = len(servers[i].Tools)
	}
	out := struct {
		Servers []MCPServerStatus `json:"servers"`
	}{Servers: servers}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("mcp: render server inventory: %w", err)
	}
	return string(pretty), nil
}
