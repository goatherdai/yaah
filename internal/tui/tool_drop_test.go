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

// TestStashedEndSettlesOnlyItsOwnKey pins the real turn boundary. The sweep no
// longer clears the stash — it can run before a start from the SAME turn is
// applied, and clearing there would strand that start (see
// TestLateToolStartAfterTheSweepIsStillSettled) — so what keeps turns apart is
// the session-unique key: another turn's start cannot match the stashed end,
// and the entry stays put for the start that can.
func TestStashedEndSettlesOnlyItsOwnKey(t *testing.T) {
	ui := gateApp()

	// An end whose start has not been applied yet: stashed.
	ui.AddToolEnd("turn-a.2", "read", "out", time.Millisecond)

	// A different turn's call is unaffected by it.
	ui.AddToolStart("turn-b.1", "read", `{"path":"a"}`)
	other := toolBlocks(ui, "turn-b.1")
	if len(other) != 1 {
		t.Fatalf("blocks for the later turn's call = %d, want 1", len(other))
	}
	if !other[0].IsRunning() {
		t.Fatal("another turn's start was settled by a stashed end it never emitted")
	}
	if n := len(ui.tr.UnmatchedEnds()); n != 1 {
		t.Fatalf("stash = %d after an unrelated start, want the entry kept for its own key", n)
	}

	// Its own key still heals, even with the sweep in between.
	ui.SweepStuckToolBlocks()
	ui.AddToolStart("turn-a.2", "read", `{"path":"b"}`)
	own := toolBlocks(ui, "turn-a.2")
	if len(own) != 1 {
		t.Fatalf("blocks for the stashed end's own call = %d, want 1", len(own))
	}
	if own[0].IsRunning() {
		t.Fatal("the late start for a stashed end created a Running block")
	}
}

// TestLostToolEndRecordIsBounded pins that the drop record cannot grow without
// bound, records one key only once, and is consumed by its reader.
func TestLostToolEndRecordIsBounded(t *testing.T) {
	ui := gateApp()
	for i := range lostToolEndsLimit + 4 {
		ui.recordLostToolEnd("run1."+strconv.Itoa(i), true)
	}
	ui.recordLostToolEnd("run1."+strconv.Itoa(lostToolEndsLimit+3), true) // duplicate

	lost := ui.takeLostToolEnds()
	if len(lost) != lostToolEndsLimit {
		t.Fatalf("recorded lost ends = %d, want the cap %d", len(lost), lostToolEndsLimit)
	}
	keys := make(map[string]bool, len(lost))
	for _, le := range lost {
		keys[le.key] = true
	}
	if keys["run1.0"] {
		t.Error("the oldest record was kept past the cap")
	}
	if !keys["run1."+strconv.Itoa(lostToolEndsLimit+3)] {
		t.Error("the newest record was dropped")
	}
	if again := ui.takeLostToolEnds(); len(again) != 0 {
		t.Errorf("a second take returned %d entries, want 0 (the record must be consumed)", len(again))
	}
}

// TestLateToolStartAfterTheSweepIsStillSettled pins the ordering the sweep must
// survive. An end can be applied before its own start, and the sweep can run
// BETWEEN them: the critical fallback runs the DoneEvent's reconciliation
// directly when the UI queue is saturated, ahead of a start still waiting in
// that queue. The stashed end must therefore survive the sweep — clearing it
// would leave the late start to create a Running block after the only sweep of
// the turn had already run, with the end's result thrown away.
//
// The order is driven explicitly (end, then Done, then start) rather than raced
// through a saturated queue, because that order is what saturation produces and
// is exactly what the fix must tolerate.
func TestLateToolStartAfterTheSweepIsStillSettled(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// 1. The end arrives with no block to receive it: it is stashed.
	ui.HandleEvent(&agent.ToolEndEvent{
		Key: "run2.1", ID: 1, Name: "read", Args: `{"path":"a"}`,
		Result: "the-result", Duration: 2 * time.Millisecond,
	})
	waitForUI(t, ui, func() bool { return len(ui.tr.UnmatchedEnds()) == 1 },
		"the end for a block that does not exist yet was not stashed")

	// 2. The turn ends and sweeps, before the start has been applied. The probe
	// rides the same queue, so it runs after the DoneEvent's reconciliation.
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	var stashedAfterSweep int
	probe := make(chan struct{})
	ui.queueUpdate(func() {
		stashedAfterSweep = len(ui.tr.UnmatchedEnds())
		close(probe)
	})
	<-probe
	if stashedAfterSweep != 1 {
		t.Fatalf("stashed ends after the sweep = %d, want 1: the sweep dropped an end a start from the same turn still needs, so that start would create an unswept Running block", stashedAfterSweep)
	}

	// 3. The late start applies: its block must be created already settled.
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run2.1", ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool { return len(toolBlocks(ui, "run2.1")) == 1 },
		"the late tool start created no block")

	var state toolblock.State
	var text string
	waitForUI(t, ui, func() bool {
		blocks := toolBlocks(ui, "run2.1")
		if len(blocks) != 1 {
			return false
		}
		state = blocks[0].S()
		text = blocks[0].SearchText()
		return true
	}, "the late tool block could not be read")

	if state == toolblock.Running {
		t.Fatal("the late start created a Running block after the turn's only sweep had run: it would keep a permanent chevron")
	}
	if state != toolblock.Done {
		t.Fatalf("late block state = %v, want Done (settled by the end that arrived first)", state)
	}
	if !strings.Contains(text, "the-result") {
		t.Fatalf("the late block lost the end's result: %q", text)
	}
}

// TestDroppedTurnEndReconciliationIsCounted pins the other half: when the
// DoneEvent's reconciliation cannot be scheduled, nothing sweeps the turn. The
// sweep cannot be run from the event goroutine (the transcript belongs to the
// UI goroutine) and re-queueing it could seal the next turn's blocks, so the
// loss is recorded rather than recovered.
func TestDroppedTurnEndReconciliationIsCounted(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	restore := saturate(t, ui)
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	restore()

	counts := ui.toolOrphanCounts()
	if got := counts[orphanKindDroppedSweep]; got != 1 {
		t.Fatalf("dropped_sweep = %d, want 1 (counts: %v)", got, counts)
	}
}

// TestLostEndSealsAStartThatLandsAfterTheSweep pins the case the sweep cannot
// reach on its own: the end's UI update was dropped while its start was still in
// flight, so at turn end there is no block to seal. The record must survive the
// sweep and be consumed by the late start, which creates the block terminal —
// otherwise the start creates a Running block after the turn's only sweep and it
// keeps a permanent chevron.
func TestLostEndSealsAStartThatLandsAfterTheSweep(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// 1. The end's update cannot be scheduled: counted, and its key recorded.
	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolEndEvent{
		Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`,
		Result: "the-result", Duration: time.Millisecond,
	})
	restore()

	// 2. The turn ends. No block exists yet, so the sweep seals nothing — but it
	// must keep the record for the start that has not landed.
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	var stashed int
	probe := make(chan struct{})
	ui.queueUpdate(func() {
		stashed = len(ui.tr.UnmatchedEnds())
		close(probe)
	})
	<-probe
	if stashed != 0 {
		t.Fatalf("the dropped end was stashed (%d entries), want the lost record path", stashed)
	}

	// 3. The start finally lands and must create the block terminal.
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool { return len(toolBlocks(ui, "run1.1")) == 1 },
		"the late start created no block")

	var state toolblock.State
	waitForUI(t, ui, func() bool {
		blocks := toolBlocks(ui, "run1.1")
		if len(blocks) != 1 {
			return false
		}
		state = blocks[0].S()
		return true
	}, "the late block could not be read")

	if state == toolblock.Running {
		t.Fatal("a start whose end was dropped landed after the turn's only sweep and was left Running: permanent chevron")
	}
	if n := runningToolBlocks(ui); n != 0 {
		t.Fatalf("still-Running blocks = %d, want 0", n)
	}

	counts := ui.toolOrphanCounts()
	if got := counts[orphanKindDroppedEnd]; got != 1 {
		t.Fatalf("dropped_end = %d, want 1 (counts: %v)", got, counts)
	}
	if got := counts[orphanKindSwept]; got != 0 {
		t.Fatalf("the sweep claimed a loss it never observed: swept = %d", got)
	}
}

// TestFallbackKeyedEndIsNotStashed pins the other half of the no-clear rule. The
// stash now survives the sweep, which is safe only because a key is
// session-unique; a keyless event falls back to the run-scoped ordinal, which
// repeats every prompt, so its end is discarded instead of hoarded.
func TestFallbackKeyedEndIsNotStashed(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// A keyless end: its block key is the run-scoped ordinal, which repeats
	// every prompt. Counted as unmatched, but taken back out of the stash.
	ui.HandleEvent(&agent.ToolEndEvent{
		ID: 1, Name: "read", Args: `{"path":"a"}`, Result: "out", Duration: time.Millisecond,
	})
	waitForUI(t, ui, func() bool {
		return len(ui.tr.UnmatchedEnds()) == 0 && ui.toolOrphanCounts()[orphanKindUnmatchedEnd] == 1
	}, "a fallback-keyed end was hoarded in the stash (an ordinal repeats every prompt, so it could settle another turn's start)")

	// The next turn reuses ordinal 1: its block must not inherit that end.
	ui.HandleEvent(&agent.ToolStartEvent{ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool {
		blocks := toolBlocks(ui, "1")
		return len(blocks) == 1 && blocks[0].IsRunning()
	}, "a fallback-keyed block was settled by an end from another turn")

	// A session-unique key is still stashed, so a merely-late start heals.
	var stashed int
	probe := make(chan struct{})
	ui.queueUpdate(func() {
		ui.AddToolEnd("turn-a.2", "read", "out", time.Millisecond)
		stashed = len(ui.tr.UnmatchedEnds())
		close(probe)
	})
	<-probe
	if stashed != 1 {
		t.Fatalf("stashed ends for a unique key = %d, want 1", stashed)
	}
}

// TestKeylessDroppedEndIsNotAlsoCountedAsSwept pins the taxonomy for the
// keyless path: a fallback-keyed loss is still a loss a seam observed, so the
// sweep that seals its block must not also claim it as swept. The record is
// turn-scoped — it can attribute this turn's sweep, but its ordinal identifies no
// later turn's call.
func TestKeylessDroppedEndIsNotAlsoCountedAsSwept(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// The keyless start lands first, so its block exists and is Running.
	ui.HandleEvent(&agent.ToolStartEvent{ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 1 },
		"the keyless start created no Running block")

	// Its end's update cannot be scheduled: one loss, observed here.
	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolEndEvent{ID: 1, Name: "read", Args: `{"path":"a"}`, Result: "ok", Duration: time.Millisecond})
	restore()

	counts := ui.toolOrphanCounts()
	if got := counts[orphanKindDroppedEnd]; got != 1 {
		t.Fatalf("dropped_end = %d, want 1 (counts: %v)", got, counts)
	}

	// The turn ends and the sweep seals the block — without double-counting.
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 0 },
		"the turn-end sweep did not seal the keyless block")

	counts = ui.toolOrphanCounts()
	if got := counts[orphanKindSwept]; got != 0 {
		t.Fatalf("the same keyless loss was counted twice: swept = %d on top of dropped_end (counts: %v)", got, counts)
	}
	if got := counts[orphanKindDroppedEnd]; got != 1 {
		t.Fatalf("dropped_end = %d after the sweep, want 1 (counts: %v)", got, counts)
	}
}

// TestDroppedTurnEndDoesNotStrandItsFallbackLosses pins the boundary a missing
// sweep leaves behind. When the DoneEvent's reconciliation cannot be scheduled,
// no sweep of that turn will ever consume its lost-end records; a fallback-keyed
// one carries a run-scoped ordinal that repeats on the next prompt, so letting it
// survive would suppress the NEXT turn's swept attribution for a block it never
// observed being lost. A unique-keyed record is kept — a start from its own turn
// may still land and consume it.
func TestDroppedTurnEndDoesNotStrandItsFallbackLosses(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	// Turn 1: a keyless end and then the turn end itself, both unschedulable.
	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolEndEvent{ID: 1, Name: "read", Args: `{"path":"a"}`, Result: "dropped", Duration: time.Millisecond})
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	restore()

	counts := ui.toolOrphanCounts()
	if counts[orphanKindDroppedEnd] != 1 || counts[orphanKindDroppedSweep] != 1 {
		t.Fatalf("turn 1 counts = %v, want one dropped_end and one dropped_sweep", counts)
	}

	// Turn 2 reuses ordinal 1 and grows a block whose end never arrives: the
	// sweep must attribute it, not the stale record from turn 1.
	ui.HandleEvent(&agent.ToolStartEvent{ID: 1, Name: "read", Args: `{"path":"b"}`})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 1 },
		"turn 2's keyless start created no Running block")
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	waitForUI(t, ui, func() bool { return runningToolBlocks(ui) == 0 },
		"turn 2's sweep did not seal the block")

	counts = ui.toolOrphanCounts()
	if got := counts[orphanKindSwept]; got != 1 {
		t.Fatalf("swept = %d, want 1: a stale fallback record from the previous turn suppressed this turn's attribution (counts: %v)", got, counts)
	}
}

// TestDroppedTurnEndKeepsUniqueLosses pins the other half: a lost end whose key
// identifies the call survives a missing sweep, because a start from that same
// turn can still land and use it.
func TestDroppedTurnEndKeepsUniqueLosses(t *testing.T) {
	ui, stop := newQueuedApp(t)
	defer stop()

	restore := saturate(t, ui)
	ui.HandleEvent(&agent.ToolEndEvent{
		Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`, Result: "the-result", Duration: time.Millisecond,
	})
	ui.HandleEvent(&agent.DoneEvent{Response: "done", ContextWindow: 8000})
	restore()

	// The turn's reconciliation was lost, but the start still lands: its block
	// must be terminal, not Running.
	ui.HandleEvent(&agent.ToolStartEvent{Key: "run1.1", ID: 1, Name: "read", Args: `{"path":"a"}`})
	waitForUI(t, ui, func() bool {
		blocks := toolBlocks(ui, "run1.1")
		return len(blocks) == 1 && !blocks[0].IsRunning()
	}, "a lost turn end stranded a keyed start as Running")
}
