package workflowpicker

import (
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/components/modal"
	"github.com/rivo/tview"
)

const modalPageName = "workflowpicker_modal"

// Entry is one selectable workflow spec: the bare name (the run handle) and
// its absolute path.
type Entry struct {
	Name string
	Path string
}

// Show displays the workflow picker over the given pages. onSelect receives
// the chosen bare name; onDismiss runs before onSelect and is also invoked on
// Escape, so callers restore their focus bookkeeping in one place. The
// returned func dismisses the picker programmatically (and is how tests
// exercise the Escape path).
func Show(app *tview.Application, pages *tview.Pages, entries []Entry, onSelect func(name string), onDismiss func(), focusAfter tview.Primitive) func() {
	closed := false
	dismiss := func() {
		if closed {
			return
		}
		closed = true
		pages.RemovePage(modalPageName)
		if focusAfter != nil {
			app.SetFocus(focusAfter)
		}
		if onDismiss != nil {
			onDismiss()
		}
	}

	list := tview.NewList().
		ShowSecondaryText(true)
	list.SetMainTextColor(tcell.ColorWhite).
		SetSecondaryTextColor(tcell.ColorGray).
		SetSelectedTextColor(tcell.ColorWhite).
		SetSelectedBackgroundColor(tcell.ColorDarkCyan)

	add := func(e Entry) {
		list.AddItem(e.Name, e.Path, 0, func() {
			dismiss()
			if onSelect != nil {
				onSelect(e.Name)
			}
		})
	}
	for _, e := range entries {
		add(e)
	}

	list.SetBorder(true).
		SetTitle(" Workflow Picker ").
		SetTitleColor(tcell.ColorYellow)

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEscape:
			dismiss()
			return nil
		}
		return ev
	})

	filter := tview.NewInputField().
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetPlaceholder("filter workflows...").
		SetPlaceholderTextColor(tcell.ColorGray)

	filter.SetChangedFunc(func(text string) {
		list.Clear()
		text = strings.ToLower(text)
		for _, e := range entries {
			if text == "" || strings.Contains(strings.ToLower(e.Name), text) || strings.Contains(strings.ToLower(e.Path), text) {
				add(e)
			}
		}
	})

	inner := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(filter, 1, 0, false).
		AddItem(list, 0, 1, true)

	inner.SetBorder(true).
		SetTitle(" Workflow Picker ").
		SetTitleColor(tcell.ColorYellow)

	pages.AddPage(modalPageName, modal.Wrap(inner), true, true)
	app.SetFocus(list)
	return dismiss
}
