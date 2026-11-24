package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPListToolSurface(t *testing.T) {
	tool := &MCPListTool{}
	if got := tool.Name(); got != "mcp" {
		t.Fatalf("Name() = %q, want %q", got, "mcp")
	}
	if got := tool.Description(); got == "" {
		t.Fatal("Description() is empty: internal/prompts/tools/mcp.md missing?")
	}
	if !json.Valid(tool.Schema()) {
		t.Fatalf("Schema() is not valid JSON: %s", tool.Schema())
	}
}

func TestMCPListToolExecute(t *testing.T) {
	populated := func() []MCPServerStatus {
		return []MCPServerStatus{
			{Name: "zebra", Transport: "http", URL: "http://localhost:7333/z", Connected: true, Tools: []string{"zebra_b", "zebra_a"}},
			{Name: "alpha", Transport: "stdio", Command: "npx -y alpha-mcp", Connected: true, Tools: []string{"alpha_x"}},
			{Name: "broken", Transport: "stdio", Command: "npx -y broken-mcp", Connected: false, Error: "spawn failed"},
		}
	}

	t.Run("servers sorted and normalized", func(t *testing.T) {
		tool := &MCPListTool{Lister: populated}
		raw, err := tool.Execute(context.Background(), "{}")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		var out struct {
			Servers []MCPServerStatus `json:"servers"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
		}
		if len(out.Servers) != 3 {
			t.Fatalf("got %d servers, want 3:\n%s", len(out.Servers), raw)
		}
		wantOrder := []string{"alpha", "broken", "zebra"}
		for i, want := range wantOrder {
			if out.Servers[i].Name != want {
				t.Errorf("servers[%d].Name = %q, want %q", i, out.Servers[i].Name, want)
			}
		}
		if out.Servers[0].Transport != "stdio" || out.Servers[0].Command != "npx -y alpha-mcp" {
			t.Errorf("alpha server = %+v, want stdio/npx -y alpha-mcp", out.Servers[0])
		}
		if out.Servers[2].URL != "http://localhost:7333/z" {
			t.Errorf("zebra URL = %q", out.Servers[2].URL)
		}
		if out.Servers[1].Connected || out.Servers[1].Error != "spawn failed" {
			t.Errorf("broken server = %+v, want unconnected with error", out.Servers[1])
		}
		// Tool names sorted, counts derived, empty list not null.
		if got := out.Servers[2].Tools; len(got) != 2 || got[0] != "zebra_a" || got[1] != "zebra_b" {
			t.Errorf("zebra tools = %v, want [zebra_a zebra_b]", got)
		}
		if out.Servers[2].ToolCount != 2 || out.Servers[0].ToolCount != 1 || out.Servers[1].ToolCount != 0 {
			t.Errorf("tool_count = %d/%d/%d, want 1/0/2", out.Servers[0].ToolCount, out.Servers[1].ToolCount, out.Servers[2].ToolCount)
		}
		if out.Servers[1].Tools == nil {
			t.Error("broken tools is null, want empty list")
		}
	})

	t.Run("nil provider result renders empty list", func(t *testing.T) {
		tool := &MCPListTool{Lister: func() []MCPServerStatus { return nil }}
		raw, err := tool.Execute(context.Background(), "")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(raw, `"servers": []`) {
			t.Errorf("want an empty servers list, got:\n%s", raw)
		}
	})

	t.Run("nil lister errors", func(t *testing.T) {
		tool := &MCPListTool{}
		if _, err := tool.Execute(context.Background(), "{}"); err == nil {
			t.Fatal("Execute with nil lister: want error, got nil")
		}
	})

	t.Run("unknown argument rejected", func(t *testing.T) {
		tool := &MCPListTool{Lister: populated}
		_, err := tool.Execute(context.Background(), `{"bogus":1}`)
		if err == nil {
			t.Fatal("Execute with unknown argument: want error, got nil")
		}
		if !strings.Contains(err.Error(), "bogus") {
			t.Errorf("error %q does not name the unknown argument", err)
		}
	})
}
