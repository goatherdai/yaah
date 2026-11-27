package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goatherdai/yaah/internal/tui/components/messages"
)

const (
	// renderWindowItems caps how many trailing conversation items a full
	// render paints (hermes MAX_MOUNTED=120): beyond the cap the older
	// items collapse behind the "earlier messages" seam and backfill on
	// scroll (see backfillWindow).
	renderWindowItems = 120

	// renderWindowRows caps the painted buffer's row count for the same
	// purpose: a window of few-but-huge items must not repaint a
	// freeze-sized buffer either.
	renderWindowRows = 2000

	// renderBackfillStep is how many older items one scroll-to-the-seam
	// prepends (hermes SLIDE_STEP=12).
	renderBackfillStep = 12
)

// markDirty sets the refresh flag. Call instead of refreshMessages()
// to coalesce multiple rapid updates into a single render pass.
func (t *App) markDirty() {
	if !t.needsRefresh.Swap(true) {
		t.requestRefresh()
	}
}

// clampRenderDelay maps a render's measured cost to the next refresh's
// minimum spacing (see requestRefresh): ten times the cost, clamped so
// cheap renders stay immediate and freeze-scale renders thin to one every
// couple of seconds.
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
	// the rendered pane stays stale forever. enqueueUIEvent falls back to
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

// buildItems materializes conversationLog[from:to] into render items,
// rendering markdown lazily with per-item width caching.
func (t *App) buildItems(from, to, w int) []messages.Item {
	items := make([]messages.Item, to-from)
	for i := from; i < to; i++ {
		ci := &t.conversationLog[i]
		var text string
		if ci.text != "" {
			if ci.isMarkdown {
				if ci.cached == "" || ci.cachedWidth != w {
					ci.cached = renderMarkdown(ci.text, w, t.Theme)
					ci.cachedWidth = w
				}
				text = ci.cached
			} else {
				text = ci.text
			}
		}
		items[i-from] = messages.Item{
			Text:      text,
			ToolBlock: ci.toolBlock,
			ReasBlock: ci.reasoningBlock,
		}
	}
	return items
}

// canAppend reports whether the conversation view is eligible for the
// incremental fast path: only new items at the tail, nothing mutated in
// place, same width, no thinking-indicator line to reconcile, and the
// window extending to the live tail — a reveal window (windowEnd >= 0)
// must not grow toward the tail one append at a time.
func (t *App) canAppend(w, n int) bool {
	return !t.needsFullRender.Load() &&
		t.renderedWidth == w &&
		t.windowEnd < 0 &&
		n > t.renderedItems &&
		t.renderedItems > 0
}

// appendChunk writes chunk onto the Messages view's incremental append
// path. Leading newlines are committed to the line index one at a time,
// each followed by a forced parse: a "\n" that is the FIRST cluster of a
// resumed parse is mis-classified by uniseg's resumed state (a width-1
// optional break instead of a mandatory break), which collapses the item
// separator into a leading space on the next item's first row. Committed
// as the buffer's trailing cluster the newline parses correctly — tview
// adjusts the trailing cluster's boundary via HasTrailingLineBreakInString
// — and the item content then resumes parsing as an ordinary cluster.
// GetWrappedLineCount is the cheap parse probe: only the empty pending
// line plus the separator get (re-)indexed, and a hard break is
// width-blind, so the probe's own width does not matter here.
func (t *App) appendChunk(chunk string) {
	for strings.HasPrefix(chunk, "\n") {
		io.WriteString(t.Messages, "\n")
		t.Messages.GetWrappedLineCount()
		chunk = chunk[1:]
	}
	if chunk != "" {
		io.WriteString(t.Messages, chunk)
	}
}

// seamText renders the dim "older messages" line painted above the
// window when earlier conversation items are collapsed behind it.
func (t *App) seamText(older int) string {
	return fmt.Sprintf("[%s]· %d earlier messages — scroll up to load more ·[-]",
		t.Theme.Dim, older)
}

// seamRows reports how many painted rows the seam occupies at width w.
func (t *App) seamRows(start, w int) int {
	if start <= 0 {
		return 0
	}
	return messages.Rows(t.seamText(start), messages.Content{Width: w, Theme: t.Theme})
}

// seamBelowText renders the dim "later messages" line painted below a
// reveal window that ends before the live tail.
func (t *App) seamBelowText(later int) string {
	return fmt.Sprintf("[%s]· %d later messages — scroll down for the live tail ·[-]",
		t.Theme.Dim, later)
}

// returnToTail returns the view to the live-tail window. The plain reset
// (userScrolled cleared, window recomputed at the next full render) is
// enough while the painted buffer still ends at the tail; a reveal
// window ends at its bottom seam instead, so the tail must be repainted
// before it can be shown.
func (t *App) returnToTail() {
	t.userScrolled = false
	t.windowStart = -1
	if t.windowEnd >= 0 {
		t.windowEnd = -1
		t.needsFullRender.Store(true)
		t.refreshMessages()
	}
}

// computeWindowStart returns the first item a full render should paint:
// the whole log when it fits the budgets, else the trailing window that
// does. Rows are accumulated tail-first over the per-item rows cache, so
// the walk costs O(window), never O(transcript).
func (t *App) computeWindowStart(n, w int) int {
	rows, count, start := 0, 0, 0
	for k := n - 1; k >= 0; k-- {
		r := t.itemRows(&t.conversationLog[k], t.buildItems(k, k+1, w)[0], w)
		if count > 0 && (count+1 > renderWindowItems || rows+r > renderWindowRows) {
			start = k + 1
			break
		}
		rows += r
		count++
	}
	return start
}

// computeWindowEnd returns the exclusive end of a window starting at
// start: forward over the per-item rows cache until the item or row
// budget binds, mirroring computeWindowStart's budgets. Always includes
// start, so end > start for a non-empty log. Callers treat end == n as
// the live tail.
func (t *App) computeWindowEnd(start, n, w int) int {
	rows, end := 0, start
	for end < n && end-start < renderWindowItems {
		r := t.itemRows(&t.conversationLog[end], t.buildItems(end, end+1, w)[0], w)
		if end > start && rows+r > renderWindowRows {
			break
		}
		rows += r
		end++
	}
	return end
}

// maybeBackfillAtSeam backfills the painted window when the viewport sits
// at its top row — the seam. This is the shared trigger for the mouse
// wheel and the keyboard scroll paths (scroll-up, page-up, Home, :top),
// so keyboard-only users can load the collapsed older items too. Being
// at the seam means the user is looking up, so it also marks the view
// scrolled — the manual window the backfill installs must not be
// re-windowed by the next full render.
func (t *App) maybeBackfillAtSeam() {
	if row, _ := t.Messages.GetScrollOffset(); row == 0 {
		t.userScrolled = true
		t.backfillWindow()
	}
}

// backfillWindow prepends renderBackfillStep older items to the painted
// window — the lazy "scroll up to load more" affordance at the seam — and
// re-renders, scrolling so the content the user was looking at stays put
// instead of sliding away beneath the freshly loaded batch. Runs from the
// mouse capture handler on the UI goroutine.
func (t *App) backfillWindow() {
	oldStart := t.windowStart
	if oldStart <= 0 {
		return
	}
	w := messageWidth(t.Messages)
	n := len(t.conversationLog)
	newStart := oldStart - renderBackfillStep
	if newStart < 0 {
		newStart = 0
	}
	if newStart == oldStart {
		return
	}
	added := 0
	for i := newStart; i < oldStart; i++ {
		added += t.itemRows(&t.conversationLog[i], t.buildItems(i, i+1, w)[0], w)
	}
	oldSeam := t.seamRows(oldStart, w)
	newSeam := t.seamRows(newStart, w)
	t.windowStart = newStart
	// A reveal window slides with the start so the painted range stays
	// inside the budgets; a tail window keeps growing toward n as
	// before.
	if t.windowEnd >= 0 {
		if end := t.computeWindowEnd(newStart, n, w); end < n {
			t.windowEnd = end
		} else {
			t.windowEnd = -1
		}
	}
	t.needsFullRender.Store(true)
	t.refreshMessages()
	t.Messages.ScrollTo(added+newSeam-oldSeam, 0)
}

// refreshMessages updates the conversation text view. When only new items
// were appended to the log since the last render, it formats just those and
// writes them incrementally; otherwise it rebuilds the buffer over the
// painted window (the trailing renderWindowItems/renderWindowRows of the
// log, with an "earlier messages" seam above it when older items are
// collapsed). This keeps per-refresh work O(new content) during streaming
// and O(window) on full rebuilds, instead of O(entire conversation). If
// the user has not scrolled up (t.userScrolled is false), it auto-scrolls
// to the end; otherwise it preserves the current viewport position.
func (t *App) refreshMessages() {
	start := time.Now()
	w := messageWidth(t.Messages)
	n := len(t.conversationLog)

	appended := false
	var formatDur, writeDur time.Duration
	var msgLen int

	// The incremental path writes only the new items' chunk (see
	// appendChunk for how leading separators commit to the line index).
	// Tool, text, and reasoning chunks are all eligible; a text chunk
	// merely costs one extra parse probe per leading separator newline.
	if t.canAppend(w, n) {
		formatStart := time.Now()
		chunk := messages.Format(t.buildItems(t.renderedItems, n, w), messages.Content{
			Width: w,
			Theme: t.Theme,
		})
		formatDur = time.Since(formatStart)

		writeStart := time.Now()
		t.appendChunk(chunk)
		writeDur = time.Since(writeStart)

		t.extendPromptAnchors(t.renderedItems, n, w)
		t.renderedItems = n
		msgLen = len(chunk)
		t.paintedLines.Add(int64(strings.Count(chunk, "\n")))
		appended = true
	} else {
		// The full rebuild paints a window of the log, not the
		// whole transcript: windowStart pins the first painted item
		// (backfill moves it; -1 means recompute — resize, compaction,
		// and clear all reset it so the window tracks the budgets). At
		// the live tail the window recomputes on every full render:
		// a pinned start would let the painted range grow past the
		// budgets across a long session and reintroduce the freeze.
		// While the user has scrolled up (backfill), the manual window
		// is kept so the full renders a running turn forces do not
		// yank the content out from under them. A reveal window
		// (windowEnd >= 0, installed by search) also ends before the
		// tail, bounded by the same budgets.
		start := t.windowStart
		if !t.userScrolled || start < 0 || start > n || w != t.windowWidth {
			start = t.computeWindowStart(n, w)
			t.windowStart = start
			t.windowWidth = w
			t.windowEnd = -1
		}
		end := n
		if e := t.windowEnd; e >= 0 && e < n {
			end = e
		}
		items := t.buildItems(start, end, w)

		msg := ""
		if start > 0 {
			msg = t.seamText(start) + "\n"
		}
		formatStart := time.Now()
		msg += messages.Format(items, messages.Content{
			Width: w,
			Theme: t.Theme,
		})
		formatDur = time.Since(formatStart)
		if end < n {
			msg += "\n" + t.seamBelowText(n-end)
		}

		writeStart := time.Now()
		t.Messages.SetText(msg)
		writeDur = time.Since(writeStart)

		t.fullRenderCount++
		t.renderedItems = end
		t.renderedWidth = w
		t.needsFullRender.Store(false)
		msgLen = len(msg)
		t.paintedLines.Store(int64(1 + strings.Count(msg, "\n")))

		t.rebuildPromptAnchors(items, w, start)
	}

	t.charsRendered.Store(int64(msgLen))

	if !t.userScrolled {
		t.Messages.ScrollToEnd()
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

	_, span := otel.Tracer("yaah").Start(context.Background(), "tui.refresh",
		trace.WithAttributes(
			attribute.Int("items", n),
			attribute.Int("msg_bytes", msgLen),
			attribute.Bool("appended", appended),
			attribute.Int("window_start", t.windowStart),
			attribute.Int("full_renders", t.fullRenderCount),
			attribute.Int64("slowlog_dropped", t.slowLog.dropped.Load()),
			attribute.Int64("slowlog_failed", t.slowLog.failed.Load()),
			attribute.Int64("dur_total_us", totalDur.Microseconds()),
			attribute.Int64("dur_format_us", formatDur.Microseconds()),
			attribute.Int64("dur_settext_us", writeDur.Microseconds()),
			attribute.Int("queue_depth", queueDepth),
			attribute.Int64("ui_event_drops", t.uiEventDrops.Load()),
			attribute.Int64("ui_event_fallbacks", t.uiEventFallbacks.Load()),
			attribute.Int64("ui_event_fallback_saturated", t.uiEventFallbackSat.Load()),
		))
	span.End()
	observability.RecordTUIRefresh(context.Background(), totalDur, cadence, queueDepth)

	if totalDur > t.slowThreshold {
		t.slowLog.printf("SLOW refreshMessages: %d items, %d bytes, appended=%v full_renders=%d total=%v format=%v write=%v",
			n, msgLen, appended, t.fullRenderCount, totalDur, formatDur, writeDur)
	}
}

// itemRows returns the item's display-row count at width w, cached on
// the convItem. In-place block mutations clear the cache; width changes
// remeasure wholesale (rowsWidth mismatch). A non-positive width is
// measured without storing: rowsWidth 0 doubles as the uncached
// sentinel, and tightening the guard keeps w<=0 (only possible via the
// 80 fallback in messageWidth, but cheap to make explicit) from
// poisoning the cache key.
func (t *App) itemRows(ci *convItem, it messages.Item, w int) int {
	if w <= 0 {
		return messages.ItemRows(it, messages.Content{Width: 80, Theme: t.Theme})
	}
	if ci.rowsWidth == w {
		return ci.rowsCache
	}
	rows := messages.ItemRows(it, messages.Content{Width: w, Theme: t.Theme})
	ci.rowsCache = rows
	ci.rowsWidth = w
	return rows
}

// rebuildPromptAnchors recomputes the anchor table from scratch during a
// full render, over the painted window only: rows before the window are
// the seam's, and prompts entirely above the window are remembered as
// windowPrePrompt so the pin still shows one at the seam. The pre-window
// prompt comes from promptIdxs by binary search, so the rebuild stays
// O(window) in item count. Row estimates use the same measure Format's
// emissions are checked against (itemRows), so counted and painted rows
// share one code path; the per-item cache keeps the walk itself cheap.
func (t *App) rebuildPromptAnchors(items []messages.Item, w, start int) {
	t.promptAnchors = t.promptAnchors[:0]
	t.windowPrePrompt = ""
	if j := sort.SearchInts(t.promptIdxs, start); j > 0 {
		t.windowPrePrompt = t.conversationLog[t.promptIdxs[j-1]].prompt
	}
	rows := t.seamRows(start, w)
	for i := range items {
		ci := &t.conversationLog[start+i]
		if ci.isPrompt {
			t.promptAnchors = append(t.promptAnchors, promptAnchor{
				item:      start + i,
				startLine: rows + 1, // the leading "\n" separator row
				input:     ci.prompt,
			})
		}
		rows += t.itemRows(ci, items[i], w)
	}
	t.renderedRows = rows
}

// extendPromptAnchors delta-appends anchors for newly rendered items on
// the incremental path, continuing from the rows already accounted for.
func (t *App) extendPromptAnchors(from, to, w int) {
	for i := from; i < to; i++ {
		ci := &t.conversationLog[i]
		if ci.isPrompt {
			t.promptAnchors = append(t.promptAnchors, promptAnchor{
				item:      i,
				startLine: t.renderedRows + 1,
				input:     ci.prompt,
			})
		}
		t.renderedRows += t.itemRows(ci, t.buildItems(i, i+1, w)[0], w)
	}
}

// reconcilePromptPin re-derives the sticky header from the scroll
// position: it shows the most recent prompt whose first row is at or
// above the current viewport top. That single rule produces the whole
// behavior — a submitted question does not take over the pin until
// auto-scroll or scrolling carries it across the top seam, the previous
// prompt stays pinned until then, and scrolling back up de-pins the
// current question and re-pins the one above it. Above the painted
// window the last pre-window prompt takes over, so the pin survives the
// seam. Runs after every app draw; an unchanged pin costs one binary
// search.
func (t *App) reconcilePromptPin() {
	want := ""
	// Above the painted window the last pre-window prompt takes over,
	// even when the window itself contains no prompts (anchors empty):
	// the pin must survive the seam in every shape.
	if t.windowStart > 0 {
		want = t.windowPrePrompt
	}
	if n := len(t.promptAnchors); n > 0 {
		row, _ := t.Messages.GetScrollOffset()
		i := sort.Search(n, func(i int) bool { return t.promptAnchors[i].startLine > row })
		if i > 0 {
			want = t.promptAnchors[i-1].input
		}
	}
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
