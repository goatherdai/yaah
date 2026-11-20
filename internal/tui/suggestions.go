package tui

import (
	"strings"

	"github.com/goatherdai/yaah/internal/tui/components/command"
)

// suggestionMinRows is the floor for the visible strip, so the highlight
// always has a usable window even on a short terminal.
const suggestionMinRows = 6

// suggestionWindow returns the half-open slice of t.suggestions to draw. The
// full match list is kept for cycling (every palette entry stays reachable),
// but the rendered height is bounded by the terminal so a bare ":" cannot
// crowd the composer off a short screen. The window follows the highlight.
// Before the first draw the screen height is unknown and the whole list shows.
func (t *App) suggestionWindow() (lo, hi int) {
	n := len(t.suggestions)
	rows := n
	if t.screenHeight > 0 {
		rows = t.screenHeight / 3
		if rows < suggestionMinRows {
			rows = suggestionMinRows
		}
		if rows > n {
			rows = n
		}
	}
	lo = t.suggestionIdx - rows/2
	if lo < 0 {
		lo = 0
	}
	if lo+rows > n {
		lo = n - rows
	}
	return lo, lo + rows
}

// updateSuggestions recomputes the typeahead strip from the composer text and
// shows, resizes or hides it. It is a no-op when the match set is unchanged so
// that typing an argument does not churn the layout. Every matching command
// is kept — a bare ":" offers the full palette, not just its first few.
func (t *App) updateSuggestions() {
	matches := command.Complete(t.Input.GetText(), command.DefaultEntries())
	if sameSuggestions(matches, t.suggestions) {
		return
	}
	t.suggestions = matches
	t.suggestionIdx = 0
	t.renderSuggestions()
}

// renderSuggestions draws the strip and sizes its row: the highlighted match in
// reverse video, the rest dim, each with its description.
func (t *App) renderSuggestions() {
	if len(t.suggestions) == 0 {
		t.Root.ResizeItem(t.suggestionsView, 0, 0)
		t.suggestionsView.SetText("")
		return
	}
	lo, hi := t.suggestionWindow()
	var b strings.Builder
	for i := lo; i < hi; i++ {
		b.WriteString(t.suggestionLine(t.suggestions[i], i == t.suggestionIdx))
		b.WriteByte('\n')
	}
	t.suggestionsView.SetText(b.String())
	t.Root.ResizeItem(t.suggestionsView, hi-lo, 0)
}

// suggestionLine renders one match. The highlighted row uses reverse video
// rather than fixed colours, so it follows the terminal's palette; with
// NO_COLOR set it degrades to a plain "> " marker.
func (t *App) suggestionLine(s command.Completion, selected bool) string {
	label := ":" + s.Label
	if t.Theme.NoColor {
		if selected {
			return "> " + label + "  " + s.Desc
		}
		return "  " + label + "  " + s.Desc
	}
	if selected {
		return "[::r] " + label + " " + t.Theme.Tag(t.Theme.Dim, s.Desc) + t.Theme.ResetTag()
	}
	return " " + t.Theme.Tag(t.Theme.Dim, label) + t.Theme.Tag(t.Theme.Dim, " "+s.Desc)
}

// hasSuggestions reports whether the typeahead strip is currently shown.
func (t *App) hasSuggestions() bool { return len(t.suggestions) > 0 }

// cycleSuggestion moves the highlight by delta, wrapping around the matches.
func (t *App) cycleSuggestion(delta int) {
	if !t.hasSuggestions() {
		return
	}
	n := len(t.suggestions)
	t.suggestionIdx = ((t.suggestionIdx+delta)%n + n) % n
	t.renderSuggestions()
}

// acceptSuggestion completes the composer with the highlighted match and hides
// the strip.
func (t *App) acceptSuggestion() bool {
	if !t.hasSuggestions() {
		return false
	}
	t.Input.SetText(t.suggestions[t.suggestionIdx].Insert, true)
	t.hideSuggestions()
	return true
}

// dismissSuggestions hides the strip. It reports whether anything was hidden, so
// Escape can dismiss typeahead without also stopping the turn.
func (t *App) dismissSuggestions() bool {
	if !t.hasSuggestions() {
		return false
	}
	t.hideSuggestions()
	return true
}

func (t *App) hideSuggestions() {
	t.suggestions = nil
	t.suggestionIdx = 0
	t.renderSuggestions()
}

// sameSuggestions reports whether a and b hold the same matches in the same
// order, so a redraw can be skipped.
func sameSuggestions(a, b []command.Completion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
