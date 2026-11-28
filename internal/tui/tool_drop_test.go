package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// tool_drop_test.go — what the view does when a tool call's UI update is not
// scheduled, and when an end is applied before its own start (yaah-77f1).
//
// enqueueUIEvent reports "scheduled", not "applied" (internal/tui/event_queue.go):
// true means the update is on the managed queue, or handed to a direct-fallback
// runner goroutine that will invoke it later; false means it will never run —
// dropped at the bounded waiter pool, or abandoned at shutdown. Only the false
// case is a lost block transition, so only that case is counted here.

// runningToolBlocks counts the transcript's still-Running tool blocks. It reads
// queue-owned state, so it belongs inside waitForUI's probe.
func runningToolBlocks(ui *App) int {
	n := 0
	for i := 0; i < ui.tr.Len(); i++ {
		if tb := ui.tr.ItemAt(i).ToolBlock; tb != nil && tb.IsRunning() {
			n++
		}
	}
	return n
}

// toolBlocks returns the transcript's tool blocks whose id is key.
func toolBlocks(ui *App, key string) []*toolblock.Block {
	var out []*toolblock.Block
	for i := 0; i < ui.tr.Len(); i++ {
		if tb := ui.tr.ItemAt(i).ToolBlock; tb != nil && tb.ID() == key {
			out = append(out, tb)
		}
	}
	return out
}

// newQueuedApp returns an app with a live consumer draining its UI event
// queue, and the func that stops that consumer.
func newQueuedApp(t *testing.T) (*App, func()) {
	t.Helper()
	ui := New("test")
	ui.bgDone = make(chan struct{})
	ui.uiEventCh = make(chan uiEvent, uiEventQueueSize)
	stop := make(chan struct{})
	drainUIEvents(ui, stop)
	return ui, func() { close(stop) }
}

// saturate drives the app into the exact state enqueueUIEvent calls a drop —
// no managed-queue slot, no free fallback runner, no free waiter slot — and
// returns the func that restores it.
//
// It is deterministic, not a race with a busy consumer: the consumer is parked
// inside an event (so nothing drains), the queue is filled to capacity, and the
// critical path's bounded fallback is held token by token. Saturated is a
// state the app can be *put* into, which is why these tests need no timing
// window.
func saturate(t *testing.T, ui *App) func() {
	t.Helper()

	parked := make(chan struct{})
	releaseConsumer := make(chan struct{})
	ui.queueUpdate(func() {
		close(parked)
		<-releaseConsumer
	})
	<-parked

	for len(ui.uiEventCh) < cap(ui.uiEventCh) {
		ui.uiEventCh <- uiEvent{fn: func() {}}
	}
	for i := 0; i < uiMaxDirectFallbacks; i++ {
		ui.fallbackSem <- struct{}{}
	}
	for i := 0; i < uiMaxDirectWaiters; i++ {
		ui.fallbackWaiters <- struct{}{}
	}

	return func() {
		t.Helper()
		close(releaseConsumer)
		for i := 0; i < uiMaxDirectFallbacks; i++ {
			<-ui.fallbackSem
		}
		for i := 0; i < uiMaxDirectWaiters; i++ {
			<-ui.fallbackWaiters
		}
		deadline := time.Now().Add(2 * time.Second)
		for len(ui.uiEventCh) > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if n := len(ui.uiEventCh); n != 0 {
			t.Fatalf("the UI queue did not drain: %d events left", n)
		}
	}
}

// TestDroppedToolEndIsCountedAsDroppedEndNotSwept pins the accounting of a
// tool end whose UI update can never be scheduled: the loss is attributed to
// the drop, at the drop, and the turn-end sweep still seals the block (so it
// never renders a permanent chevron) without claiming that loss as its own.
func TestDroppedToolEndIsCountedAsDroppedEndNotSwept(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// The call's start lands normally, so its block exists and is Running.
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 1 },
		"the tool start never created a Running block")

	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolEndEvent{
		Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`,
		Result: "ok", Duration: time.Millisecond,
	})

	counts := ui.toolOrphanCounts()
	if got := counts[orphanKindDroppedEnd]; got != 1 {
		t.Fatalf("dropped_end = %d, want 1 (counts: %v)", got, counts)
	}
	if got := counts[orphanKindSwept]; got != 0 {
		t.Fatalf("the drop was counted as swept before the sweep even ran: %v", counts)
	}

	// The turn ends; its sweep seals the block, and attributes the loss to
	// the drop that was observed rather than to its own recovery.
	restore()
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 0 },
		"the turn-end sweep did not seal the dropped-end block")

	counts = ui.toolOrphanCounts()
	if got := counts[orphanKindSwept]; got != 0 {
		t.Fatalf("the sweep claimed a loss it did not observe: swept = %d (counts: %v)", got, counts)
	}
	if got := counts[orphanKindDroppedEnd]; got != 1 {
		t.Fatalf("dropped_end = %d after the sweep, want 1 (counts: %v)", got, counts)
	}
}

// TestDroppedToolStartIsCounted pins the other lost transition: a start whose
// update can never be scheduled creates no block at all, so the drop is the
// only record of it. A delegate draws no tool block, so its loss is not
// counted as a block loss.
func TestDroppedToolStartIsCounted(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`})
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run1.2", ID: 2, Name: "delegate", Args: `{"role":"x"}`})
	restore()

	counts := ui.toolOrphanCounts()
	if got := counts[orphanKindDroppedStart]; got != 1 {
		t.Fatalf("dropped_start = %d, want 1 (counts: %v)", got, counts)
	}
	if got := len(toolBlocks(ui, "run1.1")); got != 0 {
		t.Fatalf("a block exists for a start whose update was dropped: %d", got)
	}
	if got := len(toolBlocks(ui, "run1.2")); got != 0 {
		t.Fatalf("a delegate start created a block: %d", got)
	}
}

// TestToolEndBeforeStartSettlesOneBlock pins the reordered path: the
// critical-update fallback can apply an end before an earlier queued start, so
// the block must be created from the stashed end already settled — with the
// call's own duration — instead of Running and waiting for an event that is
// already behind it.
func TestToolEndBeforeStartSettlesOneBlock(t *testing.T) {
	tests := []struct {
		name   string
		settle func(ui *App)
		state  toolblock.State
		marker string
		tag    string
	}{
		{
			name:   "success end",
			settle: func(ui *App) { ui.AddToolEnd("run1.1", "read · a", "ok", 250*time.Millisecond) },
			state:  toolblock.Done,
			marker: "✓",
			tag:    "read · a",
		},
		{
			name:   "error end",
			settle: func(ui *App) { ui.AddToolError("run1.1", "read ❌", "denied", 250*time.Millisecond) },
			state:  toolblock.Error,
			marker: "✗",
			tag:    "read ❌",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ui := gateApp()
			tc.settle(ui)
			ui.AddToolStart("run1.1", "read", `{"path":"a"}`)

			blocks := toolBlocks(ui, "run1.1")
			if len(blocks) != 1 {
				t.Fatalf("blocks for the call = %d, want 1", len(blocks))
			}
			if blocks[0].IsRunning() {
				t.Fatal("the block was created Running although its end had already arrived")
			}
			if blocks[0].S() != tc.state {
				t.Fatalf("block state = %v, want %v", blocks[0].S(), tc.state)
			}
			if n := runningToolBlocks(ui); n != 0 {
				t.Fatalf("%d block(s) left Running", n)
			}
			rendered := blocks[0].Render()
			if !strings.Contains(rendered, tc.marker) {
				t.Errorf("block not rendered terminal: %q", rendered)
			}
			if !strings.Contains(rendered, tc.tag) {
				t.Errorf("block lost the end's summary tag: %q", rendered)
			}
			if !strings.Contains(rendered, "250ms") {
				t.Errorf("block duration = %q, want the stashed end's 250ms", rendered)
			}
			// The stashed end settled this block; it must not be left over to
			// settle a later one.
			if got := len(ui.tr.UnmatchedEnds()); got != 0 {
				t.Errorf("stashed ends after the start = %d, want 0", got)
			}
		})
	}
}

// TestSweepClearsTheStashBeforeALaterStart pins the turn boundary: an end
// stashed in one turn must not settle a block created in a later one.
func TestSweepClearsTheStashBeforeALaterStart(t *testing.T) {
	ui := gateApp()

	// An end with no block at all: stashed, and then this turn's sweep runs.
	ui.AddToolEnd("run2.1", "read", "out", time.Millisecond)
	ui.SweepStuckToolBlocks()

	// The next turn reuses the key: a stale stashed end must not settle it.
	ui.AddToolStart("run2.1", "read", `{"path":"a"}`)
	blocks := toolBlocks(ui, "run2.1")
	if len(blocks) != 1 {
		t.Fatalf("blocks for the call = %d, want 1", len(blocks))
	}
	if !blocks[0].IsRunning() {
		t.Fatal("a stash cleared by the sweep still settled a later turn's block")
	}
}

// TestLostToolEndRecordIsBounded pins that the drop record cannot grow without
// bound, records one key only once, and is consumed by its reader.
func TestLostToolEndRecordIsBounded(t *testing.T) {
	ui := gateApp()
	for i := range lostToolEndsLimit + 4 {
		ui.recordLostToolEnd("run1." + strconv.Itoa(i))
	}
	ui.recordLostToolEnd("run1." + strconv.Itoa(lostToolEndsLimit+3)) // duplicate

	lost := ui.takeLostToolEnds()
	if len(lost) != lostToolEndsLimit {
		t.Fatalf("recorded lost ends = %d, want the cap %d", len(lost), lostToolEndsLimit)
	}
	if lost["run1.0"] {
		t.Error("the oldest record was kept past the cap")
	}
	if !lost["run1."+strconv.Itoa(lostToolEndsLimit+3)] {
		t.Error("the newest record was dropped")
	}
	if again := ui.takeLostToolEnds(); len(again) != 0 {
		t.Errorf("a second take returned %d entries, want 0 (the record must be consumed)", len(again))
	}
}
