package tui

import (
	"context"
	"time"

	"github.com/goatherdai/yaah/internal/observability"
	"github.com/goatherdai/yaah/internal/tui/components/activity"
)

const (
	uiEventQueueSize         = 512
	uiEventCriticalWaitLimit = 10 * time.Millisecond

	// uiMaxDirectFallbacks caps goroutines spawned by enqueueUIEventDirect.
	// tview's QueueUpdate/QueueUpdateDraw block until the main loop services
	// them, so uncapped spawning piles up blocked goroutines whenever the UI
	// thread stalls, and any straggler after App.Run returns blocks forever.
	uiMaxDirectFallbacks = 8

	// uiMaxDirectWaiters bounds goroutines waiting for a runner slot once
	// uiMaxDirectFallbacks is held (see enqueueUIEventDirect).
	uiMaxDirectWaiters = 16
)

type uiEvent struct {
	draw bool
	fn   func()
}

// startUIEventLoop starts a single consumer that serializes UI updates.
func (t *App) startUIEventLoop(done <-chan struct{}) {
	go func() {
		for {
			select {
			case <-done:
				return
			case ev := <-t.uiEventCh:
				if ev.fn == nil {
					continue
				}
				if ev.draw {
					t.App.QueueUpdateDraw(ev.fn)
				} else {
					t.App.QueueUpdate(ev.fn)
				}
			}
		}
	}()
}

func (t *App) queueUpdate(fn func()) {
	t.enqueueUIEvent(false, fn, false)
}

func (t *App) queueUpdateDraw(fn func()) {
	t.enqueueUIEvent(true, fn, false)
}

func (t *App) queueUpdateCritical(fn func()) {
	t.enqueueUIEvent(false, fn, true)
}

func (t *App) queueUpdateDrawCritical(fn func()) {
	t.enqueueUIEvent(true, fn, true)
}

func (t *App) queueThinkingUpdate(text string) {
	t.coalesceMu.Lock()
	t.pendingThinkingLabel = text
	t.thinkingSeq++
	if t.thinkingQueued {
		t.coalesceMu.Unlock()
		observability.RecordTUIQueueEvent(context.Background(), "thinking", "coalesced", t.uiQueueDepth())
		return
	}
	t.thinkingQueued = true
	t.coalesceMu.Unlock()

	t.queueUpdateDraw(func() {
		t.runThinkingUpdate()
	})
}

func (t *App) runThinkingUpdate() {
	t.coalesceMu.Lock()
	label := t.pendingThinkingLabel
	seq := t.thinkingSeq
	t.coalesceMu.Unlock()

	t.setActivity(activity.Reasoning, "")
	t.activityLine.SetPreview(label)

	t.coalesceMu.Lock()
	if seq == t.thinkingSeq {
		t.thinkingQueued = false
		t.coalesceMu.Unlock()
		return
	}
	t.coalesceMu.Unlock()

	t.queueUpdateDraw(func() {
		t.runThinkingUpdate()
	})
}

func (t *App) queueContextInfoUpdate(tokens, window int) {
	t.coalesceMu.Lock()
	t.pendingContextTokens = tokens
	t.pendingContextWindow = window
	t.contextSeq++
	if t.contextQueued {
		t.coalesceMu.Unlock()
		observability.RecordTUIQueueEvent(context.Background(), "context", "coalesced", t.uiQueueDepth())
		return
	}
	t.contextQueued = true
	t.coalesceMu.Unlock()

	t.queueUpdateDraw(func() {
		t.runContextInfoUpdate()
	})
}

func (t *App) runContextInfoUpdate() {
	t.coalesceMu.Lock()
	tokens := t.pendingContextTokens
	window := t.pendingContextWindow
	seq := t.contextSeq
	t.coalesceMu.Unlock()

	t.contextTokens = tokens
	t.contextWindow = window
	t.renderInfoPane()

	t.coalesceMu.Lock()
	if seq == t.contextSeq {
		t.contextQueued = false
		t.coalesceMu.Unlock()
		return
	}
	t.coalesceMu.Unlock()

	t.queueUpdateDraw(func() {
		t.runContextInfoUpdate()
	})
}

// enqueueUIEvent sends event work into a bounded queue. Non-critical events
// are dropped when full. Critical events wait briefly, then fall back to
// direct async QueueUpdate/QueueUpdateDraw so they are not dropped.
// It reports whether delivery was scheduled; a non-critical drop returns
// false so callers that own a "queued" flag can clear it.
func (t *App) enqueueUIEvent(draw bool, fn func(), critical bool) bool {
	if fn == nil {
		return true
	}

	t.bgMu.Lock()
	ch := t.uiEventCh
	done := t.bgDone
	depth := 0
	if ch != nil {
		depth = len(ch)
	}
	t.bgMu.Unlock()

	if ch == nil {
		observability.RecordTUIQueueEvent(context.Background(), "direct", "fallback", -1)
		t.enqueueUIEventDirect(draw, fn)
		return true
	}

	eventType := "update"
	if draw {
		eventType = "draw"
	}

	ev := uiEvent{draw: draw, fn: fn}
	select {
	case ch <- ev:
		observability.RecordTUIQueueEvent(context.Background(), eventType, "enqueued", depth)
		return true
	default:
	}

	if !critical {
		t.uiEventDrops.Add(1)
		observability.RecordTUIQueueEvent(context.Background(), eventType, "dropped", depth)
		return false
	}

	timer := time.NewTimer(uiEventCriticalWaitLimit)
	defer timer.Stop()
	select {
	case ch <- ev:
		observability.RecordTUIQueueEvent(context.Background(), eventType, "enqueued", depth)
		return true
	case <-done:
		return false
	case <-timer.C:
		t.uiEventFallbacks.Add(1)
		observability.RecordTUIQueueEvent(context.Background(), eventType, "fallback", depth)
		t.enqueueUIEventDirect(draw, fn)
		return true
	}
}

// enqueueUIEventDirect runs an event outside the managed queue. It is the
// critical-path fallback when the queue is saturated, or the path before
// Run() starts the consumer.
//
// Runner concurrency is capped by uiMaxDirectFallbacks. When the cap is
// held, the event still runs: a runner goroutine waits for a slot —
// backpressure, not loss, because this path carries critical mutators
// whose loss is the stuck-tool-block class of bug. The waiting happens on
// the runner goroutine, never on the caller: HandleEvent runs on the
// agent loop and must return immediately
// (TestHandleEvent_DoesNotBlock). uiMaxDirectWaiters bounds the runner
// pileup when the UI thread stops servicing work; beyond it events are
// dropped and counted.
func (t *App) enqueueUIEventDirect(draw bool, fn func()) {
	t.bgMu.Lock()
	done := t.bgDone
	t.bgMu.Unlock()

	// Reserve capacity: a free runner slot if one is available, else a
	// bounded waiter token, else drop.
	waiting := false
	select {
	case t.fallbackSem <- struct{}{}:
	default:
		select {
		case t.fallbackWaiters <- struct{}{}:
			waiting = true
			t.uiEventFallbackSat.Add(1)
			observability.RecordTUIQueueEvent(context.Background(), "direct", "saturated", -1)
		default:
			t.uiEventDrops.Add(1)
			observability.RecordTUIQueueEvent(context.Background(), "direct", "dropped", -1)
			return
		}
	}

	go func() {
		if waiting {
			// Upgrade the waiter token into a runner slot once one
			// frees, aborting at shutdown.
			select {
			case t.fallbackSem <- struct{}{}:
			case <-done:
				<-t.fallbackWaiters
				return
			}
			<-t.fallbackWaiters
		}
		defer func() { <-t.fallbackSem }()

		// Best-effort shutdown check: shrinks the window in which a
		// straggler blocks forever on QueueUpdate after App.Run returns.
		if done != nil {
			select {
			case <-done:
				return
			default:
			}
		}
		if draw {
			t.App.QueueUpdateDraw(fn)
		} else {
			t.App.QueueUpdate(fn)
		}
	}()
}

func (t *App) uiQueueDepth() int {
	t.bgMu.Lock()
	defer t.bgMu.Unlock()
	if t.uiEventCh == nil {
		return -1
	}
	return len(t.uiEventCh)
}
