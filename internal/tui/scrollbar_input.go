package tui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/transcript"
	"github.com/rivo/tview"
)

// scrollbar_input.go — the scrollbar's mouse driving. The geometry and
// painting live in internal/tui/transcript (scrollbar.go); this file
// only translates mouse events into viewport moves on the UI goroutine,
// riding the same seams the wheel and keyboard paths use: paging at the
// top triggers backfill, landing on the bottom seam returns to the live
// tail, and a thumb drag tracks the pointer.

// thumbDrag is a live thumb drag's anchor: pos is the thumb's virtual
// position in track rows — the viewport offset's exact fractional spot,
// not the drawn (ceil-rounded) row — and y the pointer's row at the
// last event. Anchoring the drag on the fractional position is what
// keeps a grab from lurching: the first move inside the grab row is a
// no-op, and the viewport moves in smooth pointer-row quanta
// (span/track rows per pointer row) instead of jumping to the
// integer-rounded quantum on the first motion.
type thumbDrag struct {
	pos float64
	y   int
}

// onTrack reports whether the event's position sits in the scrollbar's
// track column.
func onTrack(geom transcript.ScrollbarGeom, event *tcell.EventMouse) bool {
	px, py := event.Position()
	return px == geom.X && py >= geom.Y && py < geom.Y+geom.H
}

// scrollbarMouse routes the scrollbar's share of paneMouseCapture: the
// press picks up the thumb (or pages), moves while held drag, and any
// event without the button held ends the drag. tview fires MouseMove
// only on pointer movement and routes events to the pane only while the
// pointer is inside it, so a release that happens outside the pane is
// missed — the next event that does arrive carries no button, and the
// buttons check ends the drag (self-healing, never stuck).
func (t *App) scrollbarMouse(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	if event == nil {
		return action, event
	}
	switch action {
	case tview.MouseLeftDown:
		// A fresh press always ends any stale drag first: a release
		// outside the pane was missed by design, and the next press
		// (even in the content columns, which pass through below)
		// must not leave the old drag armed.
		t.scrollDrag = nil
		return t.scrollbarPress(event)
	case tview.MouseMove:
		if t.scrollDrag == nil {
			return action, event
		}
		return t.scrollbarDrag(event)
	case tview.MouseLeftUp:
		t.scrollDrag = nil
	}
	return action, event
}

// scrollbarPress handles MouseLeftDown: a press on the thumb grabs it,
// a press above pages up (and rides the backfill seam), a press below
// pages down. A press outside the track column — or on a thumb with no
// scroll range (content fits, or a 1-row viewport) — passes through
// untouched; a stray click must not pin the viewport against the live
// tail, so the grab branch leaves the follow state alone and the drag
// marks it only once the viewport actually moves.
func (t *App) scrollbarPress(event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	geom, ok := t.pane.ScrollbarGeom()
	if !ok || !onTrack(geom, event) || geom.ThumbSize >= geom.H {
		return tview.MouseLeftDown, event
	}
	_, py := event.Position()
	switch {
	case py >= geom.ThumbStart && py < geom.ThumbStart+geom.ThumbSize:
		span := len(t.tr.Rows()) - geom.H
		t.scrollDrag = &thumbDrag{
			pos: float64(t.pane.Offset()) * float64(geom.H-geom.ThumbSize) / float64(span),
			y:   py,
		}
	case py < geom.ThumbStart:
		t.tr.SetScrolled(true)
		t.pane.PageUp()
		t.maybeBackfillAtSeam()
	default:
		t.pane.PageDown()
		t.returnToTailAtBottom()
	}
	return tview.MouseConsumed, nil
}

// scrollbarDrag handles MouseMove while a thumb drag is active: the
// viewport tracks the pointer at span/track rows per pointer row from
// the press anchor, clamped to the row table's ends — the track's top
// row is the first row (the seam, which backfills: progressive
// disclosure, the thumb then shrinks against the longer row table) and
// its bottom row the last (which returns to live-tail follow). The
// view is marked user-scrolled only when the viewport actually moved.
func (t *App) scrollbarDrag(event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	if event.Buttons()&tcell.ButtonPrimary == 0 {
		t.scrollDrag = nil
		return tview.MouseMove, event
	}
	geom, ok := t.pane.ScrollbarGeom()
	track := geom.H - geom.ThumbSize
	span := len(t.tr.Rows()) - geom.H
	if !ok || track <= 0 || span <= 0 {
		t.scrollDrag = nil
		return tview.MouseMove, event
	}
	_, py := event.Position()
	pos := t.scrollDrag.pos + float64(py-t.scrollDrag.y)
	offset := pos * float64(span) / float64(track)
	if offset < 0 {
		offset = 0
	}
	if offset > float64(span) {
		offset = float64(span)
	}
	target := int(offset)
	before := t.pane.Offset()
	t.pane.ScrollTo(target)
	if t.pane.Offset() != before {
		t.tr.SetScrolled(true)
	}
	t.maybeBackfillAtSeam()
	t.returnToTailAtBottom()
	return tview.MouseConsumed, nil
}
