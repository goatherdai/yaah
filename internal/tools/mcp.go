package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"

	"github.com/goatherdai/yaah/internal/prompts"
)

// MCPServerStatus is the neutral per-server report the mcp tool renders.
// The wiring layer adapts live MCP clients into it, so this package never
// imports internal/mcp.
//
// Two fields carry connection details and therefore a secret-handling
// contract (review finding 3): Command is the executable only — never
// arguments, which routinely carry resolved tokens — and URL is scrubbed
// of userinfo and query by the renderer before output. Tools lists only
// names registered and callable in this session; exposed-but-unregistered
// tools are counted in UnregisteredToolCount instead (findings 9, 12).
type MCPServerStatus struct {
	Name                  string   `json:"name"`
	Transport             string   `json:"transport"`
	Command               string   `json:"command,omitempty"`
	URL                   string   `json:"url,omitempty"`
	Connected             bool     `json:"connected"`
	ToolCount             int      `json:"tool_count"` // derived from Tools on render
	Tools                 []string `json:"tools"`
	UnregisteredToolCount int      `json:"unregistered_tool_count,omitempty"`
	Error                 string   `json:"error,omitempty"`
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
		servers[i].URL = scrubURL(servers[i].URL)
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

// scrubURL removes the parts of a URL that routinely carry secrets: the
// userinfo (user:token@host) and the query string. An unparseable URL is
// dropped entirely — an unreadable connection string is never shown.
func scrubURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
