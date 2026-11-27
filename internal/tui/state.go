// state.go — focus state and query helpers.
package tui

type focusState int

const (
	focusNormal focusState = iota
	focusCommandPalette
	focusModal
)

// searchMessages finds the first transcript item whose content matches
// the query and reveals it. The search runs over the whole transcript —
// not just the painted window — because items collapsed behind the
// "earlier messages" seam still exist; a hit above the window expands
// it so the match is painted and scrolled to.
func (t *App) searchMessages(query string) {
	if k, ok := t.tr.Search(query); ok {
		t.revealItem(k)
		return
	}
	t.SetEphemeral("No matches found")
}

// revealItem paints a window around item k and scrolls the viewport to
// its first row (transcript.Model.Reveal installs the window; ItemRow
// reports the hit's exact painted row after the repaint). The repaint
// always runs so the row table provably matches the window bookkeeping
// the row math below uses.
func (t *App) revealItem(k int) {
	w := t.pane.ContentWidth()
	t.tr.Reveal(k, w)
	t.refreshMessages()
	t.pane.ScrollTo(t.tr.ItemRow(k))
	t.SetEphemeral(t.tr.ReportFound(k))
}
