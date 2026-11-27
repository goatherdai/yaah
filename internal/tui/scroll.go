package tui

import (
	"context"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/goatherdai/yaah/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// scroll.go — refresh scheduling, the paint orchestration seam, and the
// scroll-derived sticky pin. The row model itself lives in
// internal/tui/transcript: this file only decides when a repaint runs
// (markDirty/requestRefresh/flushRefresh), translate repaint stats into
// the tui.refresh telemetry span, and drive the viewport (backfill,
// return-to-tail, pin reconciliation).

// markDirty sets the refresh flag. Call instead of refreshMessages()
// to coalesce multiple rapid updates into a single render pass.
func (t *App) markDirty() {
	if !t.needsRefresh.Swap(true) {
		t.requestRefresh()
	}
}

// clampRenderDelay maps a render's measured cost to the next refresh's
// minimum spacing (see requestRefresh): ten times the cost, clamped so
// that cheap renders stay immediate and freeze-scale renders thin to one
// every couple of seconds.
func clampRenderDelay(totalDur time.Duration) time.Duration {
	d := totalDur * 10
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// requestRefresh enqueues a single refresh callback. If refresh work is
// already queued, this is a no-op. If new updates arrive while a refresh
// callback is running, another callback is scheduled after it completes.
func (t *App) requestRefresh() {
	if !t.refreshQueued.CompareAndSwap(false, true) {
		return
	}

	// Refresh is critical: dropping it would leave refreshQueued set while
	// the painted pane stays stale forever. enqueueUIEvent falls back to
	// a direct QueueUpdateDraw when the queue is saturated, and reports a
	// drop (waiter pool full, or shutdown) so the flag can be recovered.
	run := func() {
		if !t.enqueueUIEvent(true, func() {
			t.flushRefresh()
			t.refreshQueued.Store(false)
			if t.needsRefresh.Load() {
				t.requestRefresh()
			}
		}, true) {
			// The refresh was not scheduled — dropped at the waiter cap
			// or abandoned at shutdown. Clear the flag so a later caller
			// can schedule a refresh instead of being coalesced into
			// nothing.
			t.refreshQueued.Store(false)
		}
	}

	// Adaptive throttle (aider's min_delay): when the last render was
	// expensive, let a burst of flushes coalesce instead of queueing one
	// full render per flush. min_delay = clamp(last_render*10, 50ms, 2s):
	// content latency stays invisible (≤50ms while renders are fast), and
	// at freeze scale the render storm thins to one render per 2s.
	delay := time.Duration(t.renderMinDelay.Load())
	if delay <= 0 {
		run()
		return
	}
	elapsed := time.Since(time.Unix(0, t.lastRefreshUnixNano.Load()))
	if remaining := delay - elapsed; remaining > 0 {
		t.bgMu.Lock()
		done := t.bgDone
		t.bgMu.Unlock()
		time.AfterFunc(remaining, func() {
			// The throttle window can outlive the app: Stop/Run
			// teardown may have happened since this refresh was
			// scheduled. Enqueueing after the background loops are
			// stopped would take the ch==nil direct path toward a
			// stopped tview app, so clear the coalescing flag and
			// let a future Run schedule fresh work instead.
			if done != nil {
				select {
				case <-done:
					t.refreshQueued.Store(false)
					return
				default:
				}
			}
			run()
		})
		return
	}
	run()
}

// flushRefresh performs the actual render if dirty, then clears the flag.
func (t *App) flushRefresh() {
	if t.needsRefresh.Swap(false) {
		t.refreshMessages()
	}
}

// refreshMessages repaints the transcript model and keeps the viewport
// honest. The model's Repaint appends only new tail rows on the fast
// path and re-wraps the painted window otherwise (see
// transcript.Model.Repaint); this seam adds the two things the model
// must not own — the tview draw coupling and the refresh telemetry —
// and auto-scrolls to the live tail unless the user has scrolled up.
func (t *App) refreshMessages() {
	start := time.Now()

	stats := t.tr.Repaint(t.pane.ContentWidth())

	if !t.tr.UserScrolled() {
		t.pane.ScrollToEnd()
	}

	totalDur := time.Since(start)

	// Adapt the throttle to the observed render cost (see requestRefresh).
	t.renderMinDelay.Store(int64(clampRenderDelay(totalDur)))

	now := time.Now()
	var cadence time.Duration
	if prev := t.lastRefreshUnixNano.Swap(now.UnixNano()); prev > 0 {
		cadence = now.Sub(time.Unix(0, prev))
	}
	queueDepth := t.uiQueueDepth()

	// Attribute names are a dashboard contract (the TUI refresh spans
	// predate the rewrite); only the values' source moved into the
	// model's stats.
	_, span := otel.Tracer("yaah").Start(context.Background(), "tui.refresh",
		trace.WithAttributes(
			attribute.Int("items", stats.Items),
			attribute.Int("msg_bytes", stats.Bytes),
			attribute.Bool("appended", stats.Appended),
			attribute.Int("window_start", stats.WindowStart),
			attribute.Int("full_renders", stats.FullRenders),
			attribute.Int64("slowlog_dropped", t.slowLog.dropped.Load()),
			attribute.Int64("slowlog_failed", t.slowLog.failed.Load()),
			attribute.Int64("dur_total_us", totalDur.Microseconds()),
			attribute.Int64("dur_format_us", stats.WrapDur.Microseconds()),
			attribute.Int64("dur_settext_us", stats.AssembleDur.Microseconds()),
			attribute.Int("queue_depth", queueDepth),
			attribute.Int64("ui_event_drops", t.uiEventDrops.Load()),
			attribute.Int64("ui_event_fallbacks", t.uiEventFallbacks.Load()),
			attribute.Int64("ui_event_fallback_saturated", t.uiEventFallbackSat.Load()),
		))
	span.End()
	observability.RecordTUIRefresh(context.Background(), totalDur, cadence, queueDepth)

	if totalDur > t.slowThreshold {
		t.slowLog.printf("SLOW refreshMessages: %d items, %d rows, appended=%v full_renders=%d total=%v wrap=%v assemble=%v",
			stats.Items, stats.Rows, stats.Appended, stats.FullRenders, totalDur, stats.WrapDur, stats.AssembleDur)
	}
}

// repaintOnResize re-wraps the transcript when the pane's laid-out width
// diverges from the painted width, then repaints the pane into the
// current frame. The AfterDraw hook calls it — the pane's rect reflects
// the new layout only after root.Draw has cascaded it, and the hook runs
// before draw()'s screen.Show(), so the corrected rows land in the same
// frame a resize flushes. Drawing the pane directly is the one tview
// lock-free path: AfterDraw runs inside draw()'s lock, where a queued
// update would deadlock on its own completion channel.
func (t *App) repaintOnResize(screen tcell.Screen) {
	if w := t.pane.ContentWidth(); w != t.tr.PaintedWidth() {
		t.refreshMessages()
		t.pane.Draw(screen)
	}
}

// wheelStep is the rows one mouse-wheel notch scrolls the transcript.
const wheelStep = 3

// paneMouseCapture is the transcript pane's mouse handler: the wheel
// scrolls the pane for real and rides the seams — up at the top of the
// painted window backfills older items, down at the bottom returns to
// the live tail. It is a named method (not a buildUI closure) so tests
// can drive it directly. The pane is a plain Box: without this handler
// the wheel does nothing, which is the TextView behavior it replaces.
func (t *App) paneMouseCapture(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
	switch action {
	case tview.MouseScrollUp:
		t.tr.SetScrolled(true)
		t.pane.ScrollBy(-wheelStep)
		// Scrolling up at the top of the painted window means the
		// user reached the "earlier messages" seam: lazily prepend
		// older items (backfill) in the same event cycle, so the
		// wheel that asked for them also paints them.
		t.maybeBackfillAtSeam()
	case tview.MouseScrollDown:
		t.scrollPaneDown(wheelStep)
	}
	return action, event
}

// scrollPaneDown scrolls the transcript down n rows and returns the view
// to live-tail follow when the viewport lands on the bottom seam of the
// painted window. The plain scroll keeps the manual window (the user
// may still be reading above the tail); only the seam re-enters
// follow mode.
func (t *App) scrollPaneDown(n int) {
	t.pane.ScrollBy(n)
	t.returnToTailAtBottom()
}

// returnToTailAtBottom returns the view to live-tail follow when the
// viewport sits on the painted window's bottom seam; elsewhere the
// manual window stays (the user may still be reading above the tail).
func (t *App) returnToTailAtBottom() {
	if t.pane.AtBottom() {
		t.returnToTail()
	}
}

// maybeBackfillAtSeam backfills the painted window when the viewport sits
// at its top row — the seam. This is the shared trigger for the mouse
// wheel and the keyboard scroll paths (scroll-up, page-up, Home, :top),
// so keyboard-only users can load the collapsed older items too. Being
// at the seam means the user is looking up, so it also marks the view
// scrolled — the manual window the backfill installs must not be
// re-windowed by the next full repaint.
func (t *App) maybeBackfillAtSeam() {
	if t.pane.AtTop() {
		t.tr.SetScrolled(true)
		t.backfill()
	}
}

// backfill prepends one lazy batch of older items (transcript.Backfill)
// and repaints, shifting the viewport so the content the user was
// looking at stays put instead of sliding away beneath the freshly
// loaded batch. Runs from the mouse capture handler and the keyboard
// scroll paths on the UI goroutine.
func (t *App) backfill() {
	if shift := t.tr.Backfill(t.pane.ContentWidth()); shift > 0 {
		t.refreshMessages()
		t.pane.Shift(shift)
	}
}

// returnToTail returns the view to the live-tail window. The plain reset
// (userScrolled cleared, window recomputed at the next full repaint) is
// enough while the painted window still ends at the tail; a reveal
// window ends at its bottom seam instead, so the tail must be repainted
// before it can be shown.
func (t *App) returnToTail() {
	if t.tr.ReturnToTail() {
		t.refreshMessages()
	}
}

// reconcilePromptPin re-derives the sticky header from the scroll
// position via the model's anchor table (transcript.Model.PinFor). Runs
// after every app draw; an unchanged pin costs one binary search.
func (t *App) reconcilePromptPin() {
	want := t.tr.PinFor(t.pane.Offset())
	if want == t.pinnedPrompt {
		return
	}
	t.pinnedPrompt = want
	if want == "" {
		t.promptEcho.SetText("")
		t.messagesCol.ResizeItem(t.promptEcho, 0, 0)
		return
	}
	t.SetCurrentPrompt(want)
}
