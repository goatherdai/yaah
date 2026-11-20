package tui

import (
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/components/command"

	"github.com/gdamore/tcell/v2"
)

// typeText puts text in the composer and recomputes the typeahead strip, which
// is what SetText's changed handler does in the running UI.
func typeText(ui *App, text string) {
	ui.Input.SetText(text, true)
	ui.updateSuggestions()
}

func TestUpdateSuggestions_ShowsMatchesAndSizesStrip(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":st")

	if !ui.hasSuggestions() {
		t.Fatal("no suggestions for :st")
	}
	labels := make([]string, len(ui.suggestions))
	for i, s := range ui.suggestions {
		labels[i] = s.Label
	}
	for _, want := range []string{"stop", "steer"} {
		if !strings.Contains(strings.Join(labels, ","), want) {
			t.Fatalf("suggestions = %v, want %q among them", labels, want)
		}
	}
	if got := ui.suggestionsView.GetText(true); !strings.Contains(got, ":steer") {
		t.Fatalf("strip text = %q, want it to name the match", got)
	}
}

// TestUpdateSuggestions_LoneColonShowsEveryEntry pins the strip (the visible
// ":" menu) to the full palette. It used to truncate Complete's matches to
// six rows, hiding most commands until enough prefix was typed.
func TestUpdateSuggestions_LoneColonShowsEveryEntry(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":")

	entries := command.DefaultEntries()
	if len(ui.suggestions) != len(entries) {
		t.Fatalf("strip shows %d suggestions, want all %d palette entries", len(ui.suggestions), len(entries))
	}
	text := ui.suggestionsView.GetText(true)
	for _, e := range entries {
		if !strings.Contains(text, ":"+e.Label) {
			t.Errorf("strip is missing :%s", e.Label)
		}
	}
}

// TestSuggestionWindow_ShortScreenBoundsStrip keeps the full match list for
// selection while capping the drawn strip on a short terminal, and keeps the
// highlighted command visible as the user cycles to the end.
func TestSuggestionWindow_ShortScreenBoundsStrip(t *testing.T) {
	ui := newColonUI(t)
	ui.screenHeight = 12 // a short terminal: 12/3 = 4, floored to 6 rows

	typeText(ui, ":")
	if len(ui.suggestions) != len(command.DefaultEntries()) {
		t.Fatalf("match list = %d, want all %d entries", len(ui.suggestions), len(command.DefaultEntries()))
	}
	text := ui.suggestionsView.GetText(true)
	if rows := strings.Count(text, "\n"); rows > suggestionMinRows {
		t.Fatalf("strip drew %d rows on a 12-row screen, want at most %d", rows, suggestionMinRows)
	}
	if strings.Contains(text, ":"+ui.suggestions[len(ui.suggestions)-1].Label) {
		t.Fatal("strip should show the first window, not the last command")
	}

	// Cycling to the last command must scroll the window to keep it visible.
	for i := 0; i < len(ui.suggestions)-1; i++ {
		ui.cycleSuggestion(1)
	}
	text = ui.suggestionsView.GetText(true)
	last := ui.suggestions[len(ui.suggestions)-1]
	if !strings.Contains(text, ":"+last.Label) {
		t.Errorf("strip did not follow the highlight to :%s", last.Label)
	}
	if ui.suggestions[ui.suggestionIdx] != last {
		t.Errorf("highlight = %q, want the last entry", ui.suggestions[ui.suggestionIdx].Label)
	}
}

func TestUpdateSuggestions_HidesWhenNotApplicable(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":st")
	if !ui.hasSuggestions() {
		t.Fatal("precondition: suggestions should show for :st")
	}
	for _, text := range []string{":steer keep it short", "hello", ""} {
		typeText(ui, text)
		if ui.hasSuggestions() {
			t.Fatalf("suggestions still shown for %q", text)
		}
		if got := ui.suggestionsView.GetText(true); got != "" {
			t.Fatalf("strip text = %q, want empty for %q", got, text)
		}
	}
}

func TestAcceptSuggestion_CompletesCommandAndHides(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":ste")

	if !ui.acceptSuggestion() {
		t.Fatal("acceptSuggestion reported nothing to accept")
	}
	if got := ui.Input.GetText(); got != ":steer " {
		t.Fatalf("composer = %q, want %q", got, ":steer ")
	}
	if ui.hasSuggestions() {
		t.Fatal("strip still visible after accepting")
	}
}

// Enter completes a partial command from the highlighted typeahead row.
// ":st" matches stop and steer; the first is stop, which takes no argument,
// so the completed text falls through and dispatches in the same keystroke.
func TestGlobalInputCapture_EnterCompletesPartialCommand(t *testing.T) {
	ui := newColonUI(t)
	stopped := false
	ui.OnStop = func() { stopped = true }
	typeText(ui, ":st")
	if !ui.hasSuggestions() {
		t.Fatal("precondition: suggestions for :st")
	}

	enter := tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone)
	if ret := ui.globalInputCapture(enter); ret != nil {
		t.Fatal("Enter not consumed")
	}
	if !stopped {
		t.Fatalf("Enter did not dispatch the completed command; composer = %q, ephemeral = %q", ui.Input.GetText(), ui.ephemeralMsg)
	}
	if got := ui.Input.GetText(); got != "" {
		t.Fatalf("composer = %q, want cleared after dispatch", got)
	}
}

// A completion that needs an argument keeps ":name " in the composer and
// consumes Enter, so the user can type the argument instead of getting an
// "unknown command :sea" error.
func TestGlobalInputCapture_EnterOnArgCommandLeavesComposer(t *testing.T) {
	ui := newColonUI(t)
	submitted := false
	ui.OnSubmit = func(string) { submitted = true }
	typeText(ui, ":sea")

	enter := tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone)
	if ret := ui.globalInputCapture(enter); ret != nil {
		t.Fatal("Enter not consumed while an argument is pending")
	}
	if got := ui.Input.GetText(); got != ":search " {
		t.Fatalf("composer = %q, want %q", got, ":search ")
	}
	if submitted || ui.hasSuggestions() {
		t.Fatal("arg completion should neither submit nor keep the strip open")
	}
	if strings.Contains(ui.ephemeralMsg, "unknown command") {
		t.Fatalf("prefix was treated as an unknown command: %q", ui.ephemeralMsg)
	}
}

// The exact-match ranking must reach the actual Enter path. The uppercase
// case is the one that really depends on it: ":MODE" is not a literal label,
// so acceptSuggestionOnEnter completes from the highlighted row — which
// must be "mode" after ranking, not "model". (Lowercase ":mode" dispatches
// directly and only its highlight assertion depends on ordering.)
func TestGlobalInputCapture_EnterExactMatchSelectsExactCommand(t *testing.T) {
	for _, text := range []string{":mode", ":MODE"} {
		t.Run(text, func(t *testing.T) {
			ui := newColonUI(t)
			ui.OnModeSelect = func(string) {}
			typeText(ui, text)

			if got := ui.suggestions[0].Label; got != "mode" {
				t.Fatalf("highlight = %q, want mode", got)
			}
			enter := tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone)
			ui.globalInputCapture(enter)

			if ui.Pages.HasPage("modelpicker_modal") {
				t.Errorf("Enter on %s opened the model picker", text)
			}
			if !ui.Pages.HasPage("agentpicker_modal") {
				t.Errorf("Enter on %s did not open the mode picker", text)
			}
		})
	}
}

// A fully typed command is untouched: Enter still dispatches it directly
// rather than first rewriting the composer and needing a second Enter.
func TestGlobalInputCapture_EnterOnFullCommandDispatchesDirectly(t *testing.T) {
	ui := newColonUI(t)
	stopped := false
	ui.OnStop = func() { stopped = true }
	typeText(ui, ":stop")

	enter := tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone)
	ui.globalInputCapture(enter)
	if !stopped {
		t.Fatalf("Enter did not dispatch :stop; composer = %q", ui.Input.GetText())
	}
}

// A bare ":" is not a command name: Enter must not silently accept the first
// suggestion. It falls through to the ordinary prompt path as before.
func TestGlobalInputCapture_EnterOnBareColonDoesNotComplete(t *testing.T) {
	ui := newColonUI(t)
	var submitted string
	ui.OnSubmit = func(s string) { submitted = s }
	typeText(ui, ":")

	enter := tcell.NewEventKey(tcell.KeyEnter, '\r', tcell.ModNone)
	ui.globalInputCapture(enter)
	if submitted != ":" {
		t.Fatalf("submitted = %q, want %q", submitted, ":")
	}
}

func TestCycleSuggestion_WrapsBothWays(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":s")
	n := len(ui.suggestions)
	if n < 2 {
		t.Fatalf("need at least two matches for :s, got %d", n)
	}
	if ui.suggestionIdx != 0 {
		t.Fatalf("initial index = %d, want 0", ui.suggestionIdx)
	}
	ui.cycleSuggestion(1)
	if ui.suggestionIdx != 1 {
		t.Fatalf("after down: index = %d, want 1", ui.suggestionIdx)
	}
	ui.cycleSuggestion(-1)
	if ui.suggestionIdx != 0 {
		t.Fatalf("after up: index = %d, want 0", ui.suggestionIdx)
	}
	ui.cycleSuggestion(-1)
	if ui.suggestionIdx != n-1 {
		t.Fatalf("wrapped index = %d, want %d", ui.suggestionIdx, n-1)
	}
}

// Tab accepts the highlighted match while the strip is up, and Tab is passed
// through untouched when it is not.
func TestGlobalInputCapture_TabAcceptsSuggestion(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":ste")

	tab := tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)
	if ret := ui.globalInputCapture(tab); ret != nil {
		t.Fatal("Tab not consumed while the strip was visible")
	}
	if got := ui.Input.GetText(); got != ":steer " {
		t.Fatalf("composer = %q, want %q", got, ":steer ")
	}

	if ret := ui.globalInputCapture(tab); ret == nil {
		t.Fatal("Tab consumed with no suggestions visible")
	}
}

func TestGlobalInputCapture_ArrowsCycleOnlyWhileVisible(t *testing.T) {
	ui := newColonUI(t)
	typeText(ui, ":s")

	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	if ret := ui.globalInputCapture(down); ret != nil {
		t.Fatal("Down not consumed while the strip was visible")
	}
	if ui.suggestionIdx != 1 {
		t.Fatalf("index = %d, want 1", ui.suggestionIdx)
	}

	ui.hideSuggestions()
	if ret := ui.globalInputCapture(down); ret == nil {
		t.Fatal("Down consumed with no suggestions visible")
	}
}

// Escape must dismiss the strip without stopping the turn — the user is editing
// a command name, not asking to abort.
func TestGlobalInputCapture_EscapeDismissesSuggestionsBeforeStopping(t *testing.T) {
	ui := newColonUI(t)
	stopped := false
	ui.OnStop = func() { stopped = true }
	typeText(ui, ":st")

	esc := tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone)
	if ret := ui.globalInputCapture(esc); ret != nil {
		t.Fatal("Esc not consumed while dismissing the strip")
	}
	if ui.hasSuggestions() {
		t.Fatal("strip still visible after Esc")
	}
	if stopped {
		t.Fatal("Esc stopped the turn instead of just dismissing the strip")
	}

	// A second Esc, with nothing to dismiss, does stop the turn.
	if ret := ui.globalInputCapture(esc); ret != nil {
		t.Fatal("second Esc not consumed")
	}
	if !stopped {
		t.Fatal("second Esc did not stop the turn")
	}
}

// The highlighted row must not depend on a hardcoded colour pair: with NO_COLOR
// it degrades to a plain marker.
func TestSuggestionLine_NoColorUsesMarker(t *testing.T) {
	ui := newColonUI(t)
	ui.Theme.NoColor = true

	sel := ui.suggestionLine(command.Completion{Label: "steer", Desc: "Inject"}, true)
	if !strings.HasPrefix(sel, "> ") || strings.Contains(sel, "[") {
		t.Fatalf("selected line = %q, want a plain '> ' marker with no colour tags", sel)
	}
	plain := ui.suggestionLine(command.Completion{Label: "steer", Desc: "Inject"}, false)
	if strings.Contains(plain, "[::r]") {
		t.Fatalf("unselected line = %q, want no colour tags", plain)
	}
}
