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
	_, my, _, _ := ui.Messages.GetInnerRect()
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

	ui.toggleBanner()
	if ui.headerHeight() != 0 {
		t.Errorf("headerHeight after :banner = %d, want 0", ui.headerHeight())
	}
	ui.toggleBanner()
	if ui.headerHeight() != ui.bannerLines+1 {
		t.Errorf("headerHeight after re-show = %d, want %d", ui.headerHeight(), ui.bannerLines+1)
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
	_, my, _, _ := ui.Messages.GetInnerRect()
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
