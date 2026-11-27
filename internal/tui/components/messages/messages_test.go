package messages

import (
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/reasoning"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
	"github.com/goatherdai/yaah/internal/tui/lolcat"
)

func TestRender_Empty(t *testing.T) {
	th := colors.NewDarkTheme()
	if out := Render(Item{}, colors.RenderCtx{Width: 80, Theme: &th}); out != "" {
		t.Errorf("an empty item should emit nothing, got %q", out)
	}
}

func TestRender_TextOnly(t *testing.T) {
	th := colors.NewDarkTheme()
	rctx := colors.RenderCtx{Width: 80, Theme: &th}
	out := Render(Item{Text: "Hello, world"}, rctx)
	if !strings.Contains(out, "Hello, world") {
		t.Errorf("should contain text, got %q", out)
	}
	if !strings.HasPrefix(out, "\n") || !strings.HasSuffix(out, "\n\n") {
		t.Errorf("text emission must open with a separator and close with a blank hand-off, got %q", out)
	}
}

func TestRender_ToolBlock(t *testing.T) {
	th := colors.NewDarkTheme()
	tb := toolblock.New("t1", "bash", `{"command":"ls"}`, &th)
	tb.Complete("done", "output")
	out := Render(Item{ToolBlock: tb}, colors.RenderCtx{Width: 80, Theme: &th})
	if !strings.Contains(out, "bash") {
		t.Errorf("should contain tool name, got %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("tool emission must end with a newline, got %q", out)
	}
}

func TestRender_ReasoningBlock(t *testing.T) {
	th := colors.NewDarkTheme()
	rb := reasoning.New("r1", "model thoughts", 0, &th)
	out := Render(Item{ReasBlock: rb}, colors.RenderCtx{Width: 80, Theme: &th})
	plain := lolcat.StripTags(out)
	if !strings.Contains(plain, "Reasoning") {
		t.Errorf("should contain reasoning label, got %q", plain)
	}
}

func TestRender_MixedBlocks(t *testing.T) {
	th := colors.NewDarkTheme()
	tb := toolblock.New("t1", "read", `{"path":"/foo"}`, &th)
	tb.Complete("done", "content")
	rb := reasoning.New("r1", "thoughts", 0, &th)
	rctx := colors.RenderCtx{Width: 80, Theme: &th}

	out := Render(Item{Text: "User: do something"}, rctx)
	if !strings.Contains(out, "User: do something") {
		t.Error("should contain user text")
	}
	if !strings.Contains(Render(Item{ToolBlock: tb}, rctx), "read") {
		t.Error("should contain tool")
	}
	if plain := lolcat.StripTags(Render(Item{ReasBlock: rb}, rctx)); !strings.Contains(plain, "Reasoning") {
		t.Error("should contain reasoning")
	}
}

func TestRender_WidthPropagation(t *testing.T) {
	th := colors.NewDarkTheme()
	tb := toolblock.New("t1", "bash", `{"command":"ls"}`, &th)
	tb.Complete("done", "out")
	tb.Toggle()
	wide := Render(Item{ToolBlock: tb}, colors.RenderCtx{Width: 120, Theme: &th})
	narrow := Render(Item{ToolBlock: tb}, colors.RenderCtx{Width: 40, Theme: &th})
	if len(wide) <= len(narrow) {
		t.Error("wider context should produce longer output")
	}
}
