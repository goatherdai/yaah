package tui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/components/activity"
	"github.com/goatherdai/yaah/internal/tui/components/banner"
	"github.com/goatherdai/yaah/internal/tui/components/infopane"
	"github.com/goatherdai/yaah/internal/tui/components/input"
	localTodo "github.com/goatherdai/yaah/internal/tui/components/todo"
	"github.com/rivo/tview"
)

// buildUI wires the component tree and layout.
func (t *App) buildUI() {
	var bannerLines int
	bannerLines, t.Banner = banner.Build(t.Theme.Dim)
	// The transcript pane replaces a TextView: the model's wrapped-row
	// table is painted per visible row (transcript.Pane.Draw), so the
	// paint cost is O(viewport) instead of O(buffer) per frame. The
	// mouse capture rides the pane's Box seam; tview forwards wheel
	// actions to every child's handler, so the capture fires wherever
	// the wheel spins — the same exposure the TextView path had.
	t.pane.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseScrollUp:
			t.tr.SetScrolled(true)
			// Scrolling up at the top of the painted window means the
			// user reached the "earlier messages" seam: lazily prepend
			// older items (backfill) in the same event cycle, so the
			// wheel that asked for them also paints them.
			t.maybeBackfillAtSeam()
		case tview.MouseScrollDown:
			// Back at the bottom of the painted window: return to the
			// live tail (a reveal window's bottom seam means the tail
			// itself still needs painting; returnToTail decides whether
			// a repaint is needed).
			if t.pane.AtBottom() {
				t.returnToTail()
			}
		}
		return action, event
	})

	// Prompt input: bordered TextArea with pink border. Grows up to
	// promptMaxContentLines content rows as the user types, then the
	// TextArea scrolls internally.
	prompt := input.BuildPrompt(t.Theme)
	t.Input = prompt.TextArea
	t.prompt = prompt
	prompt.TextArea.SetChangedFunc(func() {
		t.growPromptInput()
		t.updateSuggestions()
	})

	// Activity line: spinner/gauge + state label, always reserved.
	t.activityLine = activity.NewRow(t.Theme)

	// Typeahead strip: zero height until a colon command name is being typed,
	// then grows to fit the matches.
	t.suggestionsView = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)

	// Sticky prompt echo: shows the current user prompt at the top of
	// the messages pane so it never scrolls away. Wraps up to
	// promptEchoMaxLines rows; longer prompts truncate with "…".
	t.promptEcho = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(true).
		SetWordWrap(true)
	t.promptEcho.SetBackgroundColor(tcell.ColorBlack)

	t.InfoPane = infopane.Build(t.Theme.InfoPaneBorder)
	t.TodoPane = localTodo.Build(nil, t.Theme.TasksPaneBorder)
	t.BackgroundJobsPane = tview.NewTextView().
		SetDynamicColors(true).
		SetWordWrap(true)
	t.BackgroundJobsPane.SetBorder(true)
	t.BackgroundJobsPane.SetTitle(" Subagents ")
	t.BackgroundJobsPane.SetBorderColor(tcell.GetColor(t.Theme.SubAgentsBorder))
	t.BackgroundJobsPane.SetTitleColor(tcell.GetColor(t.Theme.SubAgentsBorder))

	headerRows := bannerLines
	totalRows := headerRows
	rows := make([]int, totalRows)
	for i := range rows {
		rows[i] = 1
	}

	t.Header = tview.NewGrid().
		SetRows(rows...).
		SetColumns(-1).
		SetBorders(false)

	t.Header.AddItem(t.Banner, 0, 0, headerRows, 1, 0, 0, false)

	// One blank row between the sticky prompt echo and the conversation
	// pane, so agent content never butts up against the question line
	// as it scrolls beneath it.
	messagesCol := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(t.promptEcho, 1, 0, false).
		AddItem(tview.NewBox(), 1, 0, false).
		AddItem(t.pane, 0, 1, false)
	t.messagesCol = messagesCol

	rightPane := tview.NewFlex().
		SetDirection(tview.FlexRow)
	rightPane.AddItem(t.InfoPane, 0, 3, false)
	rightPane.AddItem(t.BackgroundJobsPane, 0, 0, false)
	rightPane.AddItem(t.TodoPane, 0, 1, false)
	t.rightPane = rightPane

	body := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(messagesCol, 0, 4, true).
		AddItem(rightPane, 0, 1, false)

	t.Root = tview.NewFlex().
		SetDirection(tview.FlexRow).
		SetFullScreen(true)

	t.Root.AddItem(t.Header, headerRows, 0, false)
	t.Root.AddItem(body, 0, 1, true)
	t.Root.AddItem(t.activityLine, 1, 0, false)
	t.Root.AddItem(t.suggestionsView, 0, 0, false)
	t.Root.AddItem(prompt, 3, 0, true)

	t.Pages = tview.NewPages()
	t.Pages.AddPage("main", t.Root, true, true)
	t.App.SetRoot(t.Pages, true).EnableMouse(true)

	// The sticky prompt header is scroll-derived: re-evaluate it after
	// every draw so pin swaps track both auto-scroll and manual scrolling.
	// The resize seam rides the same hook: the transcript's rows are
	// wrapped once at the painted width, not re-wrapped by every draw the
	// way a TextView is, so after a resize (which lays out inside
	// root.Draw, below) the pane's width and the model's painted width
	// diverge — re-wrap and queue one more frame. The correction is
	// one frame late by construction: BeforeDraw cannot see the new
	// layout, and the rows cannot be wrapped before it exists.
	t.App.SetAfterDrawFunc(func(screen tcell.Screen) {
		// Track the real screen height so the typeahead can bound its
		// rendered window on short terminals. Zero until the first draw
		// means "unknown", and the strip shows every match.
		if _, h := screen.Size(); h > 0 {
			t.screenHeight = h
		}
		t.repaintOnResize(screen)
		t.reconcilePromptPin()
	})

	t.App.SetInputCapture(t.globalInputCapture)

	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	tview.Styles.PrimaryTextColor = tcell.ColorWhite
	tview.Styles.BorderColor = tcell.ColorGray
	tview.Styles.TitleColor = tcell.ColorYellow
	tview.Styles.ContrastBackgroundColor = tcell.ColorDarkCyan
	tview.Styles.MoreContrastBackgroundColor = tcell.ColorDarkCyan
	tview.Styles.ContrastSecondaryTextColor = tcell.ColorWhite
}
