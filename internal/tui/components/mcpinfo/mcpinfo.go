// Package mcpinfo formats the MCP server status section.
package mcpinfo

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/modal"
	"github.com/rivo/tview"
)

// Server holds a single MCP server state.
type Server struct {
	Name      string
	Connected bool
}

// Format returns a formatted MCP server status block.
func Format(servers []Server, th *colors.Theme) string {
	if len(servers) == 0 {
		return th.Tag(th.Dim, "  (no servers)\n")
	}

	var b strings.Builder
	for _, s := range servers {
		if s.Connected {
			b.WriteString(fmt.Sprintf("  %s %s\n",
				th.Tag(th.Connected, "●"),
				th.Tag(th.Detail, s.Name),
			))
		} else {
			b.WriteString(fmt.Sprintf("  %s %s\n",
				th.Tag(th.Dim, "●"),
				th.Tag(th.Dim, s.Name),
			))
		}
	}
	return b.String()
}

const modalPageName = "mcp_modal"

// Show displays the MCP server status as a modal overlay — the same block
// the info pane carries, promoted to the center so :mcp answers the
// question "which servers is this session talking to" without hunting the
// side pane. onDismiss is called when the overlay is dismissed (Escape or
// Enter).
func Show(app *tview.Application, pages *tview.Pages, servers []Server, th *colors.Theme, onDismiss func()) {
	connected := 0
	for _, s := range servers {
		if s.Connected {
			connected++
		}
	}
	text := fmt.Sprintf("%s connected of %d configured\n\n%s",
		th.Tag(th.Detail, fmt.Sprint(connected)),
		len(servers),
		Format(servers, th),
	)
	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(text)
	textView.SetBorder(true).
		SetTitle(" MCP Servers ").
		SetTitleColor(tcell.ColorYellow)
	// The capture sits on the focused leaf (the textView), the same shape
	// the pickers use. tview 0.42 also consults a capture on the wrapping
	// flex (the page's WrapInputHandler), but the leaf is where the key
	// arrives first; TestShow_EscapeAndEnterDismiss pins both dismissal
	// keys against the real event loop.
	textView.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter {
			pages.RemovePage(modalPageName)
			onDismiss()
			return nil
		}
		return ev
	})

	flex := modal.Wrap(textView)

	pages.AddPage(modalPageName, flex, true, true)
	app.SetFocus(textView)
}
