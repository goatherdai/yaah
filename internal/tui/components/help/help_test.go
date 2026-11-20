package help

import (
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/components/command"
	"github.com/rivo/tview"
)

// TestCommandLinesMatchPalette pins the help overlay's ":command" menu to
// the command palette's single source of truth. The list used to be
// hand-maintained and silently omitted :agent and :mode when those were
// added to the palette. Any future palette entry now fails this test until
// the help menu picks it up (which it does automatically, being derived).
func TestCommandLinesMatchPalette(t *testing.T) {
	entries := command.DefaultEntries()
	lines := CommandLines(entries)
	if len(lines) != len(entries) {
		t.Fatalf("help rendered %d command lines, want %d", len(lines), len(entries))
	}
	for i, e := range entries {
		if !strings.Contains(lines[i], ":"+e.Label) {
			t.Errorf("help line %d = %q, want it to name :%s", i, lines[i], e.Label)
		}
		if !strings.Contains(lines[i], e.Desc) {
			t.Errorf("help line %d = %q, want the palette description %q", i, lines[i], e.Desc)
		}
	}
}

func TestCommandLinesUseAccurateUsage(t *testing.T) {
	// The rendered lines carry tview style tags and escaped brackets;
	// unescape first so the assertion checks what the user actually sees.
	rendered := strings.Join(CommandLines(command.DefaultEntries()), "\n")
	lines := tview.Unescape(rendered)
	for _, want := range []string{
		":steer <text>", ":followup <text>", ":search <query>",
		":model [name]", ":agent [name]", ":mode [read-only|full]",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("help menu is missing usage %q (rendered %q)", want, rendered)
		}
	}
	// The old hand-maintained list rendered ":model <name>" as if the
	// argument were required; Usage says it is optional.
	if strings.Contains(lines, ":model <name>") {
		t.Error("help menu still shows the inaccurate ':model <name>' usage")
	}
}

// TestCommandLinesEscapeUsageTags pins the tview-tag escape: without it a
// dynamic-colour TextView consumes "[name]" as a style tag and the help
// overlay shows ":model" / ":agent" with no argument hint at all.
func TestCommandLinesEscapeUsageTags(t *testing.T) {
	rendered := strings.Join(CommandLines(command.DefaultEntries()), "\n")
	for _, raw := range []string{":model [name]", ":agent [name]"} {
		if strings.Contains(rendered, raw) {
			t.Errorf("usage %q was not escaped for tview: %q", raw, rendered)
		}
	}
	for _, escaped := range []string{":model [name[]", ":agent [name[]"} {
		if !strings.Contains(rendered, escaped) {
			t.Errorf("help menu did not escape %q: %q", escaped, rendered)
		}
	}
}
