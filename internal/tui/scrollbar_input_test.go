package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// scrollbarTestSetup grows a scrollable transcript, lays the UI out, and
// draws once so the pane's offset snaps to the live tail — the state a
// real user's first scrollbar interaction starts from. Returns the
// current scrollbar geometry.
func scrollbarTestSetup(t *testing.T, ui *App, sim tcell.Screen) (int, int) {
	t.Helper()
	growForScroll(t, ui, 200)
	ui.Root.Draw(sim)
	geom, ok := ui.pane.ScrollbarGeom()
	if !ok {
		t.Fatalf("pane reports no scrollbar geometry")
	}
	return geom.X, geom.Y
}

// pressAt injects a left-button press at (x, y) and returns the capture's
// verdict.
func pressAt(ui *App, x, y int) (tview.MouseAction, *tcell.EventMouse) {
	return ui.paneMouseCapture(tview.MouseLeftDown, tcell.NewEventMouse(x, y, tcell.ButtonPrimary, tcell.ModNone))
}

// dragTo injects a pointer move to (x, y) with the button held.
func dragTo(ui *App, x, y int) (tview.MouseAction, *tcell.EventMouse) {
	return ui.paneMouseCapture(tview.MouseMove, tcell.NewEventMouse(x, y, tcell.ButtonPrimary, tcell.ModNone))
}

// TestScrollbarPressGrabsThumb pins the grab contract: a press on the
// thumb is consumed, the drag state records the grab distance, and the
// first move without the button held ends the drag without consuming —
// a release outside the pane self-heals on the next event.
func TestScrollbarPressGrabsThumb(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	x, y := scrollbarTestSetup(t, ui, sim)

	geom, _ := ui.pane.ScrollbarGeom()
	thumbY := geom.ThumbStart + geom.ThumbSize/2
	act, ev := pressAt(ui, x, thumbY)
	if act != tview.MouseConsumed || ev != nil {
		t.Fatalf("press on thumb not consumed: act=%v ev=%v", act, ev)
	}
	if ui.scrollDrag != thumbY-geom.ThumbStart {
		t.Fatalf("grab distance = %d, want %d", ui.scrollDrag, thumbY-geom.ThumbStart)
	}

	// A move without the button held ends the drag and passes through.
	act, ev = ui.paneMouseCapture(tview.MouseMove, tcell.NewEventMouse(x, y, 0, tcell.ModNone))
	if act != tview.MouseMove || ev == nil {
		t.Fatalf("buttonless move during drag should pass through: act=%v", act)
	}
	if ui.scrollDrag >= 0 {
		t.Fatalf("drag did not end on the buttonless move")
	}
}

// TestScrollbarDragScrollsAndDiscloses pins the drag path end to end:
// dragging the grabbed thumb to the track's top lands on the seam and
// backfills older items (the thumb shrinks against the longer row
// table instead of claiming unpainted rows), and dragging to the
// bottom seam returns to live-tail follow.
func TestScrollbarDragScrollsAndDiscloses(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	x, y := scrollbarTestSetup(t, ui, sim)

	geom, _ := ui.pane.ScrollbarGeom()
	if _, ev := pressAt(ui, x, geom.ThumbStart); ev != nil {
		t.Fatalf("press on thumb not consumed")
	}
	rowsBefore := len(ui.tr.Rows())

	// Drag to the track's top: the viewport lands on the seam and the
	// backfill extends the painted window in the same event — the
	// shift keeps the content under the pointer put, so the offset
	// after the event is the backfill shift, not 0.
	if _, ev := dragTo(ui, x, y); ev != nil {
		t.Fatalf("drag move not consumed")
	}
	if ui.pane.Offset() <= 0 {
		t.Fatalf("drag to the seam left offset %d, want the backfill shift", ui.pane.Offset())
	}
	if rowsAfter := len(ui.tr.Rows()); rowsAfter <= rowsBefore {
		t.Fatalf("drag to the seam did not backfill: rows %d -> %d", rowsBefore, rowsAfter)
	}
	if !ui.tr.UserScrolled() {
		t.Fatalf("drag left the view in tail-follow mode")
	}

	// Dragging at the seam again discloses the next batch: the thumb
	// shrinks against the longer row table instead of claiming
	// unpainted rows.
	rowsBefore = len(ui.tr.Rows())
	if _, ev := dragTo(ui, x, y); ev != nil {
		t.Fatalf("drag move at the seam not consumed")
	}
	if rowsAfter := len(ui.tr.Rows()); rowsAfter <= rowsBefore {
		t.Fatalf("second drag at the seam did not backfill: rows %d -> %d", rowsBefore, rowsAfter)
	}

	// Drag back to the bottom seam: live-tail follow resumes.
	if _, ev := dragTo(ui, x, y+geom.H-1); ev != nil {
		t.Fatalf("drag move not consumed")
	}
	if ui.tr.UserScrolled() {
		t.Fatalf("drag to the bottom seam did not return to tail follow")
	}
}

// TestScrollbarPageClicks pins the groove: a press above the thumb pages
// up (and rides the backfill seam), a press below pages down, and a
// press in the content columns passes through untouched.
func TestScrollbarPageClicks(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	x, y := scrollbarTestSetup(t, ui, sim)

	geom, _ := ui.pane.ScrollbarGeom()
	h := paneViewportHeight(t, ui)
	tail := ui.pane.Offset()

	// Press above the thumb: one page up, consumed.
	act, ev := pressAt(ui, x, geom.ThumbStart-1)
	if act != tview.MouseConsumed || ev != nil {
		t.Fatalf("press above thumb not consumed: act=%v ev=%v", act, ev)
	}
	if got := ui.pane.Offset(); got > tail-h {
		t.Fatalf("press above thumb moved to %d, want <= %d", got, tail-h)
	}
	if !ui.tr.UserScrolled() {
		t.Fatalf("page-up click did not mark the view scrolled")
	}

	// Press below the thumb: one page down.
	geom, _ = ui.pane.ScrollbarGeom()
	if geom.ThumbStart+geom.ThumbSize < y+geom.H {
		act, _ = pressAt(ui, x, geom.ThumbStart+geom.ThumbSize)
		if act != tview.MouseConsumed {
			t.Fatalf("press below thumb not consumed")
		}
		if got := ui.pane.Offset(); got <= tail-h {
			t.Fatalf("press below thumb did not page down: offset %d", got)
		}
	}

	// A press in the content columns is not the scrollbar's business.
	ui.scrollDrag = -1
	act, ev = pressAt(ui, x-1, y)
	if act != tview.MouseLeftDown || ev == nil {
		t.Fatalf("content press should pass through: act=%v", act)
	}
	if ui.scrollDrag >= 0 {
		t.Fatalf("content press must not start a drag")
	}
}

// TestScrollbarFitsContentPressPassesThrough pins the fits-content
// contract: when the transcript fits the viewport the thumb fills the
// track, there is no scroll range, and a track press passes through
// without marking the view user-scrolled — a stray click must not pin
// the viewport against the live tail.
func TestScrollbarFitsContentPressPassesThrough(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	ui.appendMessage("one short message")
	ui.refreshMessages()
	ui.Root.Draw(sim)

	geom, ok := ui.pane.ScrollbarGeom()
	if !ok {
		t.Fatalf("pane reports no scrollbar geometry")
	}
	if geom.ThumbSize != geom.H {
		t.Fatalf("fits-content thumb size = %d, want full track %d", geom.ThumbSize, geom.H)
	}
	act, ev := pressAt(ui, geom.X, geom.Y)
	if act != tview.MouseLeftDown || ev == nil {
		t.Fatalf("fits-content track press should pass through: act=%v", act)
	}
	if ui.tr.UserScrolled() {
		t.Fatalf("fits-content press marked the view user-scrolled")
	}
	if ui.scrollDrag >= 0 {
		t.Fatalf("fits-content press armed a drag")
	}
}

// TestScrollbarStaleDragHealsOnPress pins the self-heal's second path:
// a release outside the pane is missed, so the drag flag can be stale
// when the next press lands in the content columns — the press must
// clear it, or a later button-held move would jump the viewport.
func TestScrollbarStaleDragHealsOnPress(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	x, y := scrollbarTestSetup(t, ui, sim)

	geom, _ := ui.pane.ScrollbarGeom()
	if _, ev := pressAt(ui, x, geom.ThumbStart); ev != nil {
		t.Fatalf("press on thumb not consumed")
	}
	if ui.scrollDrag < 0 {
		t.Fatalf("press did not arm the drag")
	}

	// The stale release is missed; the next press lands in the content
	// columns and must disarm the drag.
	if _, ev := pressAt(ui, x-1, y); ev == nil {
		t.Fatalf("content press should pass through")
	}
	if ui.scrollDrag >= 0 {
		t.Fatalf("content press left a stale drag armed")
	}

	// A button-held move afterwards is an ordinary move, not a drag:
	// the viewport does not move.
	before := ui.pane.Offset()
	act, ev := dragTo(ui, x-1, y+2)
	if act != tview.MouseMove || ev == nil {
		t.Fatalf("move after healed drag should pass through: act=%v", act)
	}
	if got := ui.pane.Offset(); got != before {
		t.Fatalf("healed drag still scrolled the viewport: %d -> %d", before, got)
	}
}
