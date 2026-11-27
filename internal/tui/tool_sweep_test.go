package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent"
)

// TestDoneEventSweepsStuckToolBlocks pins the 0.222.1 stuck-chevron
// finding end to end: a tool call whose end event never arrives (dropped
// or reordered) must not render a permanent Running chevron — the turn's
// DoneEvent seals it terminal with the interrupted marker.
func TestDoneEventSweepsStuckToolBlocks(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.ToolStartEvent{ID: 7, Name: "bash", Args: `{"command":"ls"}`})
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})

	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "◌")
	}, "the stuck block was not swept to the interrupted marker")
	waitForUI(t, ui, func() bool {
		return !strings.Contains(ui.tr.PaintedText(), "▶")
	}, "a running chevron survived the turn")
}

// TestDoneEventLeavesSettledBlocks pins the sweep's honesty: blocks that
// did receive their end event keep their own terminal state through the
// turn-end sweep.
func TestDoneEventLeavesSettledBlocks(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.ToolStartEvent{ID: 1, Name: "read", Args: `{"path":"a"}`})
	ui.HandleEvent(&agent.ToolEndEvent{ID: 1, Name: "read", Args: `{"path":"a"}`, Result: "ok", Duration: time.Millisecond})
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})

	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "✓")
	}, "the completed block was not rendered done")
	waitForUI(t, ui, func() bool {
		return !strings.Contains(ui.tr.PaintedText(), "◌")
	}, "a settled block was disturbed by the sweep")
}

// TestUnmatchedToolEndIsCountedNotFatal pins the unmatched-end path: an
// end event for an unknown block must not panic or crash the app; the
// drift is surfaced on the orphan counter instead.
func TestUnmatchedToolEndIsCountedNotFatal(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.ToolEndEvent{ID: 999, Name: "read", Args: `{}`, Result: "ok", Duration: time.Millisecond})
	ui.HandleEvent(&agent.DoneEvent{Response: "all settled", ContextWindow: 8000})

	// Liveness, not emptiness: the DoneEvent's response must still land,
	// proving the unmatched end neither crashed nor wedged the queue.
	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "all settled")
	}, "the unmatched end event wedged the queue")
}

// waitForUI polls cond until it holds or the deadline expires. cond runs
// through the UI event queue (like probeEphemeral) because the transcript
// is QueueUpdateDraw-only state: reading it from the test goroutine
// would race the drain goroutine's repaints.
func waitForUI(t *testing.T, ui *App, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := make(chan bool, 1)
		ui.queueUpdate(func() { got <- cond() })
		select {
		case v := <-got:
			if v {
				return
			}
		case <-time.After(time.Second):
			t.Fatal("probe never serviced")
		}
	}
	t.Fatal(msg)
}
