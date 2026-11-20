// Package help renders the help overlay showing keybindings and commands.
//
// Triggered by ":help" (the palette's help entry).
package help

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/components/command"
	"github.com/goatherdai/yaah/internal/tui/components/modal"
	"github.com/rivo/tview"
)

// Binding is a keybinding entry for the help overlay.
type Binding struct {
	Label    string
	HelpText string
}

const modalPageName = "help_modal"

// CommandLines renders the ":command" reference from the command palette's
// entries. Deriving it from DefaultEntries keeps the help menu and the
// palette in lockstep: ":agent" and ":mode" used to be missing here because
// the list was hand-maintained. Usage() supplies the argument form for
// arg-taking commands, so ":model [name]" is accurate too.
func CommandLines(entries []command.Entry) []string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		spec := ":" + e.Label
		if u := command.Usage(e.Cmd); u != "" {
			spec = u
		}
		lines = append(lines, fmt.Sprintf("  [white]%-22s[-] [dim]%s[-]", spec, e.Desc))
	}
	return lines
}

// Show displays the help overlay with keybindings and command reference.
// onDismiss is called when the overlay is dismissed (Escape or Enter).
func Show(app *tview.Application, pages *tview.Pages, bindings []Binding, entries []command.Entry, onDismiss func()) {
	var lines []string
	lines = append(lines, "[yellow]Keyboard Shortcuts[-]\n\n")
	for _, b := range bindings {
		lines = append(lines, fmt.Sprintf("  [white]%-12s[-] [dim]%s[-]", b.Label, b.HelpText))
	}
	lines = append(lines, "\n[yellow]Commands (Ctrl+P → :command)[-]\n")
	lines = append(lines, CommandLines(entries)...)

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(strings.Join(lines, "\n"))
	textView.SetBorder(true).
		SetTitle(" Help ").
		SetTitleColor(tcell.ColorYellow)

	flex := modal.Wrap(textView)

	flex.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter {
			pages.RemovePage(modalPageName)
			onDismiss()
			return nil
		}
		return ev
	})

	pages.AddPage(modalPageName, flex, true, true)
	app.SetFocus(textView)
}
