package tui

import (
	"testing"
	"time"
)

// waitForRefreshed gives the throttle test generous margins so runner
// jitter cannot flip the assertions.

func waitForRefreshed(t *testing.T, ui *App, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for ui.needsRefresh.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("refresh did not run within %s", within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAdaptiveThrottleCoalescesDuringSlowRenders(t *testing.T) {
	ui := newTestUI()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	// Simulate an expensive last render: the throttle window is 200ms.
	ui.renderMinDelay.Store(int64(200 * time.Millisecond))
	ui.lastRefreshUnixNano.Store(time.Now().UnixNano())

	ui.appendMessage("throttle check") // markDirty → throttled requestRefresh

	// Well inside the window the refresh must not have run yet.
	time.Sleep(50 * time.Millisecond)
	if !ui.needsRefresh.Load() {
		t.Fatal("refresh ran inside the throttle window")
	}
	// Past the window the delayed callback must have flushed it.
	waitForRefreshed(t, ui, time.Second)
}

func TestRefreshImmediateWhenRenderFast(t *testing.T) {
	ui := newTestUI()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	// No recorded render cost: no throttle window.
	ui.renderMinDelay.Store(0)
	ui.appendMessage("fast check")
	waitForRefreshed(t, ui, time.Second)
}

func TestThrottleAdaptsToRenderCost(t *testing.T) {
	ui := New("test")
	sim := setupSim(t, ui)
	growSession(t, ui, 30)

	// A real render stores a floor-bounded, cap-bounded delay...
	sim.SetSize(100, 40)
	ui.Root.SetRect(0, 0, 100, 40)
	ui.Root.Draw(sim)
	ui.refreshMessages()
	if got := time.Duration(ui.renderMinDelay.Load()); got < 50*time.Millisecond || got > 2*time.Second {
		t.Fatalf("after a real render, min_delay = %s, want within [50ms, 2s]", got)
	}

	// ...and a fresh app without a recorded cost starts unthrottled.
	fresh := New("test")
	if got := time.Duration(fresh.renderMinDelay.Load()); got != 0 {
		t.Fatalf("fresh app min_delay = %s, want 0", got)
	}
}

func TestClampRenderDelay(t *testing.T) {
	for _, tc := range []struct{ cost, want time.Duration }{
		{0, 50 * time.Millisecond},
		{3 * time.Millisecond, 50 * time.Millisecond},
		{100 * time.Millisecond, time.Second},
		{300 * time.Millisecond, 2 * time.Second},
		{5 * time.Second, 2 * time.Second},
	} {
		if got := clampRenderDelay(tc.cost); got != tc.want {
			t.Errorf("clampRenderDelay(%s) = %s, want %s", tc.cost, got, tc.want)
		}
	}
}

func TestCriticalEventWaitsInsteadOfDropping(t *testing.T) {
	if testing.Short() {
		t.Skip("critical-event gate: skipped in -short")
	}
	ui := New("test")
	sim := setupSim(t, ui)
	for i := 0; i < uiMaxDirectFallbacks; i++ {
		ui.fallbackSem <- struct{}{}
	}

	ran := make(chan struct{})
	// Saturated: the caller returns immediately (HandleEvent runs on the
	// agent loop and must not block), but the event is not dropped — a
	// runner goroutine waits for a slot.
	ui.enqueueUIEventDirect(false, func() { close(ran) })
	if got := ui.uiEventFallbackSat.Load(); got != 1 {
		t.Fatalf("uiEventFallbackSat = %d, want 1", got)
	}

	select {
	case <-ran:
		t.Fatal("event ran despite a saturated semaphore")
	case <-time.After(100 * time.Millisecond):
	}

	// Run the real loop so a freed slot lets the waiter execute.
	<-ui.fallbackSem
	ui.App.SetScreen(sim)
	runErr := make(chan error, 1)
	go func() { runErr <- ui.App.Run() }()
	t.Cleanup(func() {
		ui.App.Stop()
		if err := <-runErr; err != nil {
			t.Errorf("event loop: %v", err)
		}
	})

	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("critical event never executed after a slot freed")
	}
}

func TestEnqueueUIEventDirect_WaitersCapped(t *testing.T) {
	ui := New("test")
	ui.bgDone = make(chan struct{})
	for i := 0; i < uiMaxDirectFallbacks; i++ {
		ui.fallbackSem <- struct{}{}
	}
	for i := 0; i < uiMaxDirectWaiters; i++ {
		ui.fallbackWaiters <- struct{}{}
	}

	before := ui.uiEventDrops.Load()
	start := time.Now()
	ui.enqueueUIEventDirect(false, func() {})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("enqueueUIEventDirect blocked on a full waiter pool, took %v", elapsed)
	}
	if got := ui.uiEventDrops.Load(); got != before+1 {
		t.Fatalf("uiEventDrops = %d, want %d", got, before+1)
	}
}

func TestEnqueueUIEventDirect_WaiterAbortsAtShutdown(t *testing.T) {
	ui := New("test")
	done := make(chan struct{})
	ui.bgDone = done
	for i := 0; i < uiMaxDirectFallbacks; i++ {
		ui.fallbackSem <- struct{}{}
	}

	ran := make(chan struct{})
	// Saturated: the event goes to a waiter goroutine blocked on
	// fallbackSem.
	ui.enqueueUIEventDirect(false, func() { close(ran) })
	if got := ui.uiEventFallbackSat.Load(); got != 1 {
		t.Fatalf("uiEventFallbackSat = %d, want 1", got)
	}

	close(done)

	// The waiter must abort without running the event and release its
	// waiter token so the bounded pool is not leaked.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-ui.fallbackWaiters:
		default:
			if time.Now().After(deadline) {
				t.Fatal("waiter token never released after shutdown")
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	select {
	case <-ran:
		t.Fatal("waiter ran the event after shutdown")
	default:
	}
}

func TestPaintedLinesTracksTranscript(t *testing.T) {
	ui := New("test")
	setupSim(t, ui)

	ui.appendMessage("line one")
	ui.refreshMessages()
	want := int64(len(ui.tr.Rows()))
	if got := ui.tr.PaintedLines(); got != want {
		t.Fatalf("paintedLines = %d, want %d", got, want)
	}

	// A second refresh re-derives the count from the new row table.
	ui.appendMessage("line two")
	ui.refreshMessages()
	want = int64(len(ui.tr.Rows()))
	if got := ui.tr.PaintedLines(); got != want {
		t.Fatalf("paintedLines = %d, want %d", got, want)
	}
}
