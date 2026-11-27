package tui

import (
	"testing"
	"time"
)

// drainUIEvents consumes the app's UI event queue and runs each fn
// synchronously, standing in for the real tview main loop in tests.
func drainUIEvents(ui *App, done <-chan struct{}) {
	go func() {
		for {
			select {
			case <-done:
				return
			case ev := <-ui.uiEventCh:
				if ev.fn != nil {
					ev.fn()
				}
			}
		}
	}()
}

// probeEphemeral reads ephemeralMsg through the UI queue so the access
// is serialized exactly as it is in production (QueueUpdateDraw-only
// field per the App contract).
func probeEphemeral(t *testing.T, ui *App) string {
	t.Helper()
	got := make(chan string, 1)
	ui.queueUpdate(func() { got <- ui.ephemeralMsg })
	select {
	case v := <-got:
		return v
	case <-time.After(time.Second):
		t.Fatal("probe never serviced")
		return ""
	}
}

func newTestUI() *App {
	ui := New("test")
	ui.bgDone = make(chan struct{})
	ui.uiEventCh = make(chan uiEvent, 16)
	return ui
}

func TestSetEphemeral_StaleTimerDoesNotClearNewerMessage(t *testing.T) {
	oldTTL := ephemeralTTL
	ephemeralTTL = 30 * time.Millisecond
	t.Cleanup(func() { ephemeralTTL = oldTTL })

	ui := newTestUI()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.SetEphemeral("first")
	time.Sleep(5 * time.Millisecond) // let gen-1 timer get close to firing
	ui.SetEphemeral("second")

	// Wait well past both timers.
	time.Sleep(3 * ephemeralTTL)

	if got := probeEphemeral(t, ui); got != "" {
		t.Fatalf("ephemeralMsg = %q, want cleared after TTL", got)
	}
}

func TestSetEphemeral_ClearsAfterTTL(t *testing.T) {
	oldTTL := ephemeralTTL
	ephemeralTTL = 30 * time.Millisecond
	t.Cleanup(func() { ephemeralTTL = oldTTL })

	ui := newTestUI()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.SetEphemeral("only")
	time.Sleep(3 * ephemeralTTL)

	if got := probeEphemeral(t, ui); got != "" {
		t.Fatalf("ephemeralMsg = %q, want empty after TTL", got)
	}
}

// TestPromptEchoSeamSpacer pins the one-row gap between the sticky prompt
// echo and the messages buffer: scrolled conversation content must never
// butt up against the user's question line.
func TestPromptEchoSeamSpacer(t *testing.T) {
	ui := New("test")
	ui.SetCurrentPrompt("q")
	sim := newPaneSim(t, 120, 30)
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)

	_, ey, _, eh := ui.promptEcho.GetRect()
	_, my, _, _ := ui.pane.GetInnerRect()
	if gap := my - (ey + eh); gap != 1 {
		t.Errorf("expected exactly 1 spacer row between prompt echo and messages pane, got %d", gap)
	}
}

// TestHeaderPaddingRow pins the blank row inside the header beneath the
// banner tagline: it ships with the banner and dies with it under :banner.
func TestHeaderPaddingRow(t *testing.T) {
	ui := New("test")
	sim := newPaneSim(t, 120, 30)
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)

	_, hy, _, hh := ui.Header.GetRect()
	_, by, _, bh := ui.Banner.GetInnerRect()
	if hh != ui.bannerLines+1 {
		t.Errorf("header height = %d, want bannerLines+1 = %d", hh, ui.bannerLines+1)
	}
	if gap := (hy + hh) - (by + bh); gap != 1 {
		t.Errorf("expected exactly 1 padding row below the banner, got %d", gap)
	}
	sim.Show()
	rows := paintedRows(sim, 120, 30)
	if pad := by + bh; pad < len(rows) && rows[pad] != "" {
		t.Errorf("padding row below the banner is not blank: %q", rows[pad])
	}

	// The pane's top while the banner is shown, to compare against the
	// hidden layout below.
	_, shownPaneY, _, _ := ui.pane.GetInnerRect()

	ui.toggleBanner()
	if ui.headerHeight() != 0 {
		t.Errorf("headerHeight after :banner = %d, want 0", ui.headerHeight())
	}
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)
	if _, _, _, hh := ui.Header.GetRect(); hh != 0 {
		t.Errorf("header still painted after :banner: height %d", hh)
	}
	_, hiddenPaneY, _, _ := ui.pane.GetInnerRect()
	if hiddenPaneY >= shownPaneY {
		t.Errorf("hiding the header did not move the pane up: %d >= %d", hiddenPaneY, shownPaneY)
	}

	ui.toggleBanner()
	if ui.headerHeight() != ui.bannerLines+1 {
		t.Errorf("headerHeight after re-show = %d, want %d", ui.headerHeight(), ui.bannerLines+1)
	}
	// The re-shown header must reclaim its leading slot, not append
	// after the composer at the root flex's end.
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)
	hx, hy, _, _ := ui.Header.GetRect()
	if hy != 0 || hx != 0 {
		t.Errorf("re-shown header at (%d,%d), want the root's top-left", hx, hy)
	}
	if _, ny, _, _ := ui.pane.GetInnerRect(); ny != shownPaneY {
		t.Errorf("re-shown header moved the pane: top row %d, want %d", ny, shownPaneY)
	}
}

// TestPromptSeamRule pins the separator rule under the sticky prompt
// pin: one painted row while the pin is up, nothing once it clears, and
// the rule — not a blank row — is what the conversation scrolls under.
func TestPromptSeamRule(t *testing.T) {
	ui := New("test")
	ui.SetCurrentPrompt("q")
	sim := newPaneSim(t, 120, 30)
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)

	rx, ry, rw, rh := ui.promptRule.GetRect()
	if rh != 1 || rw <= 0 {
		t.Fatalf("rule rect while the pin is shown = %dx%d at (%d,%d), want 1 row", rw, rh, rx, ry)
	}
	_, ey, _, eh := ui.promptEcho.GetRect()
	_, my, _, _ := ui.pane.GetInnerRect()
	if ry != ey+eh {
		t.Errorf("rule row %d does not abut the pin's bottom row %d", ry, ey+eh)
	}
	if my != ry+1 {
		t.Errorf("messages top row %d does not abut the rule row %d", my, ry)
	}
	sim.Show()
	cells, w, _ := sim.GetContents()
	for x := rx; x < rx+rw; x++ {
		i := ry*w + x
		if i >= len(cells) || len(cells[i].Runes) == 0 || cells[i].Runes[0] != '─' {
			t.Fatalf("rule row %d col %d is not a separator rune", ry, x)
		}
	}

	ui.SetCurrentPrompt("")
	ui.Root.Draw(sim)
	_, _, _, rh = ui.promptRule.GetRect()
	if rh != 0 {
		t.Errorf("rule height after the pin clears = %d, want 0", rh)
	}
}

// TestPanePaintsChromeBackground pins the 0.223.0 background regression:
// the transcript pane must paint the same background as every other
// element — its Box's construction-time color, the one the old TextView
// painted — not the live PrimitiveBackgroundColor global, which buildUI
// overrides to transparent after the chrome is built (the conversation
// became the one region showing the terminal through).
func TestPanePaintsChromeBackground(t *testing.T) {
	ui := New("test")
	sim := newPaneSim(t, 120, 30)
	ui.Root.SetRect(0, 0, 120, 30)
	ui.Root.Draw(sim)
	sim.Show()

	px, py, pw, ph := ui.pane.GetInnerRect()
	cells, w, _ := sim.GetContents()
	want := ui.pane.GetBackgroundColor()
	for _, pos := range [][2]int{
		{px + pw - 1, py},          // right edge of the first row
		{px + pw - 1, py + ph - 1}, // bottom-right corner
		{px, py + ph - 1},          // bottom-left corner
	} {
		i := pos[1]*w + pos[0]
		if i < 0 || i >= len(cells) {
			t.Fatalf("sample position (%d,%d) outside the screen", pos[0], pos[1])
		}
		_, got, _ := cells[i].Style.Decompose()
		if got != want {
			t.Fatalf("pane cell (%d,%d) paints background %v, want the chrome background %v",
				pos[0], pos[1], got, want)
		}
	}
}
