package tui

import (
	"fmt"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// growForScroll fills the transcript past every window budget and paints
// it, leaving the viewport at the live tail.
func growForScroll(t *testing.T, ui *App, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()
}

// paneViewportHeight returns the pane's laid-out inner height — the page
// size PageUp/PageDown scroll by.
func paneViewportHeight(t *testing.T, ui *App) int {
	t.Helper()
	_, _, _, h := ui.pane.GetInnerRect()
	if h <= 0 {
		t.Fatalf("pane has no laid-out height")
	}
	return h
}

// TestPaneWheelScrollsViewport pins the mouse seam on the custom Pane:
// the wheel must move the viewport (the plain Box underneath never
// would), scroll-up marks the view scrolled, and scroll-down back at
// the bottom seam returns to live-tail follow.
func TestPaneWheelScrollsViewport(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growForScroll(t, ui, 200)

	tail := ui.pane.Offset()
	ui.paneMouseCapture(tview.MouseScrollUp, nil)
	if got := ui.pane.Offset(); got != tail-wheelStep {
		t.Fatalf("wheel-up moved the viewport to %d, want %d", got, tail-wheelStep)
	}
	if !ui.tr.UserScrolled() {
		t.Fatalf("wheel-up did not mark the view scrolled")
	}

	ui.paneMouseCapture(tview.MouseScrollDown, nil)
	if got := ui.pane.Offset(); got != tail {
		t.Fatalf("wheel-down moved the viewport to %d, want %d", got, tail)
	}
	if ui.tr.UserScrolled() {
		t.Fatalf("wheel-down at the bottom seam did not return to tail follow")
	}
}

// TestKeyboardScrollActions pins the keyboard scroll path: PgUp/PgDn
// move the viewport and are consumed, Home lands on the seam (and
// backfills), End returns to the tail, and a modal focus passes the
// scroll keys through untouched.
func TestKeyboardScrollActions(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growForScroll(t, ui, 200)
	h := paneViewportHeight(t, ui)

	tail := ui.pane.Offset()
	if ev := ui.globalInputCapture(tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("PgUp was not consumed")
	}
	if got := ui.pane.Offset(); got != tail-h {
		t.Fatalf("PgUp moved the viewport to %d, want %d", got, tail-h)
	}

	if ev := ui.globalInputCapture(tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("PgDn was not consumed")
	}
	if got := ui.pane.Offset(); got != tail {
		t.Fatalf("PgDn moved the viewport to %d, want %d", got, tail)
	}
	if ui.tr.UserScrolled() {
		t.Fatalf("PgDn back at the bottom seam did not return to tail follow")
	}

	// Home lands on the seam; with older items collapsed behind it the
	// backfill paints one batch and keeps the viewport at the top.
	if ev := ui.globalInputCapture(tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("Home was not consumed")
	}
	if got := ui.pane.Offset(); got != 0 {
		t.Fatalf("Home moved the viewport to %d, want 0", got)
	}

	if ev := ui.globalInputCapture(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)); ev != nil {
		t.Fatalf("End was not consumed")
	}
	if got := ui.pane.Offset(); got != len(ui.tr.Rows())-h {
		t.Fatalf("End moved the viewport to %d, want %d", got, len(ui.tr.Rows())-h)
	}

	// A modal owns the navigation keys: scroll actions must pass
	// through instead of scrolling the transcript underneath.
	tail = ui.pane.Offset()
	ui.focus = focusModal
	if ev := ui.globalInputCapture(tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone)); ev == nil {
		t.Fatalf("PgUp must pass through to a modal")
	}
	if got := ui.pane.Offset(); got != tail {
		t.Fatalf("PgUp leaked to the transcript under a modal: %d != %d", got, tail)
	}
	ui.focus = focusNormal
}

// TestArrowKeysRespectComposer pins the arrow split: an empty composer's
// Up/Down scroll the transcript, while a composer holding text — single-
// or multi-row — keeps the arrows for its cursor and the keys pass
// through untouched.
func TestArrowKeysRespectComposer(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growForScroll(t, ui, 200)
	tail := ui.pane.Offset()

	// Empty composer: Up and Down scroll and are consumed.
	up := tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	if ev := ui.globalInputCapture(up); ev != nil {
		t.Fatalf("Up on an empty composer was not consumed")
	}
	if got := ui.pane.Offset(); got != tail-1 {
		t.Fatalf("Up moved the viewport to %d, want %d", got, tail-1)
	}
	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	if ev := ui.globalInputCapture(down); ev != nil {
		t.Fatalf("Down on an empty composer was not consumed")
	}
	if got := ui.pane.Offset(); got != tail {
		t.Fatalf("Down moved the viewport to %d, want %d", got, tail)
	}

	// Text in the composer: both arrows pass through to the cursor.
	for _, tc := range []struct {
		name string
		text string
		end  bool
	}{
		{"single row", ":steer ", true},
		{"multi row", "first\nsecond\nthird", true},
		{"multi row, cursor at start", "first\nsecond\nthird", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui.Input.SetText(tc.text, tc.end)
			if ev := ui.globalInputCapture(up); ev == nil {
				t.Fatalf("Up must pass through while the composer holds text")
			}
			if ev := ui.globalInputCapture(down); ev == nil {
				t.Fatalf("Down must pass through while the composer holds text")
			}
			if got := ui.pane.Offset(); got != tail {
				t.Fatalf("arrows moved the viewport under a text-bearing composer: %d", got)
			}
		})
	}
}

// TestWheelDispatchThroughRoot pins the tview dispatch contract, not just
// the capture: the wheel event must reach the pane's capture through the
// real flex chain AND be consumed — tview redraws after a mouse event only
// when a handler claims it, so an unconsumed scroll lands but never paints
// (the 0.223.0 regression: the wheel looked dead while keyboard scroll,
// whose key events always trigger a draw, worked).
func TestWheelDispatchThroughRoot(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	growForScroll(t, ui, 200)
	ui.Root.Draw(sim)

	tail := ui.pane.Offset()
	x, y, _, _ := ui.pane.GetInnerRect()
	ev := tcell.NewEventMouse(x+5, y+5, tcell.WheelUp, tcell.ModNone)
	handler := ui.Root.MouseHandler()
	if handler == nil {
		t.Fatalf("root has no mouse handler")
	}
	consumed, _ := handler(tview.MouseScrollUp, ev, func(tview.Primitive) {})
	if !consumed {
		t.Fatalf("wheel event was not consumed; tview would not redraw")
	}
	if got := ui.pane.Offset(); got != tail-wheelStep {
		t.Fatalf("wheel through the dispatch chain moved the viewport to %d, want %d", got, tail-wheelStep)
	}
}
