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
// tail, and a thumb drag positions the viewport absolutely.

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
		return t.scrollbarPress(event)
	case tview.MouseMove:
		if t.scrollDrag < 0 {
			return action, event
		}
		return t.scrollbarDrag(event)
	case tview.MouseLeftUp:
		t.scrollDrag = -1
	}
	return action, event
}

// scrollbarPress handles MouseLeftDown: a press on the thumb grabs it
// (the drag keeps the grab distance, so the thumb moves with the
// pointer rather than jumping under it), a press above or below pages
// one viewport and rides the seams exactly like the keyboard paging
// path. A press outside the track column passes through untouched.
func (t *App) scrollbarPress(event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	geom, ok := t.pane.ScrollbarGeom()
	if !ok || !onTrack(geom, event) {
		return tview.MouseLeftDown, event
	}
	t.tr.SetScrolled(true)
	_, py := event.Position()
	switch {
	case py >= geom.ThumbStart && py < geom.ThumbStart+geom.ThumbSize:
		t.scrollDrag = py - geom.ThumbStart
	case py < geom.ThumbStart:
		t.pane.PageUp()
		t.maybeBackfillAtSeam()
	default:
		t.pane.PageDown()
		t.returnToTailAtBottom()
	}
	return tview.MouseConsumed, nil
}

// scrollbarDrag handles MouseMove while a thumb drag is active: the
// viewport follows the pointer absolutely, the top seam backfills
// (progressive disclosure — the thumb then shrinks against the longer
// row table), and landing on the bottom seam returns to live-tail
// follow like every other scroll path.
func (t *App) scrollbarDrag(event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	if event.Buttons()&tcell.ButtonPrimary == 0 {
		t.scrollDrag = -1
		return tview.MouseMove, event
	}
	_, py := event.Position()
	t.tr.SetScrolled(true)
	t.pane.ScrollToThumb(py - t.scrollDrag)
	t.maybeBackfillAtSeam()
	t.returnToTailAtBottom()
	return tview.MouseConsumed, nil
}
