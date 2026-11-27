package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/components/command"
)

// TestRender_WindowBoundsPaint pins the windowed painter at session scale:
// a full render paints the trailing window (last renderWindowItems, row-
// budgeted), collapses everything older behind the seam, and the painted
// buffer no longer contains the early turns.
func TestRender_WindowBoundsPaint(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// ~4-5 items per turn: 60 turns overflows the 120-item window.
	growSession(t, ui, 60)

	ui.refreshMessages()
	if ui.windowStart <= 0 {
		t.Fatalf("window did not engage: windowStart=%d, log=%d items", ui.windowStart, len(ui.conversationLog))
	}
	rendered := ui.Messages.GetText(true)
	if !strings.Contains(rendered, fmt.Sprintf("turn %d: investigate", 59)) {
		t.Fatalf("window lost the newest turn")
	}
	if strings.Contains(rendered, "turn 0: investigate") {
		t.Fatalf("window painted the oldest turn: the seam did not collapse it")
	}
	if !strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam line missing from the painted buffer")
	}
}

// TestRender_BackfillSlidesWindow pins the lazy backfill: prepending
// renderBackfillStep older items paints them, keeps the seam until the
// window covers the whole log, and scrolls so the content the user was
// looking at stays put.
func TestRender_BackfillSlidesWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// 200 small distinctive items: the item cap binds (rows stay well
	// under budget), so the window is exactly the last 120 items.
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	if ui.windowStart != 80 {
		t.Fatalf("windowStart=%d, want 80 (200 items, 120-item window)", ui.windowStart)
	}
	if ui.windowStart <= 0 {
		t.Fatalf("window did not engage")
	}
	rendered := ui.Messages.GetText(true)
	if !strings.Contains(rendered, "item 199") || !strings.Contains(rendered, "item 080") {
		t.Fatalf("window lost the newest items")
	}
	if strings.Contains(rendered, "item 079") {
		t.Fatalf("items above the window are painted")
	}

	// The user is at the seam (scroll offset 0); backfill one step.
	ui.userScrolled = true
	ui.backfillWindow()

	if ui.windowStart != 80-renderBackfillStep {
		t.Fatalf("backfill moved windowStart %d -> %d, want %d", 80, ui.windowStart, 80-renderBackfillStep)
	}
	rendered = ui.Messages.GetText(true)
	if !strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam must persist while older items remain")
	}
	if !strings.Contains(rendered, "item 068") {
		t.Fatalf("backfill did not paint the newly loaded items")
	}
	// Viewport stability: the offset advanced past the seam so the row
	// the user was looking at stays put.
	if row, _ := ui.Messages.GetScrollOffset(); row <= 0 {
		t.Fatalf("backfill left the viewport at the seam: offset=%d", row)
	}

	// Backfilling to the top removes the seam and paints item 000.
	for ui.windowStart > 0 {
		prev := ui.windowStart
		ui.backfillWindow()
		if ui.windowStart >= prev {
			t.Fatalf("backfill stalled at %d", prev)
		}
	}
	rendered = ui.Messages.GetText(true)
	if strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam must vanish once the window covers the whole log")
	}
	if !strings.Contains(rendered, "item 000") {
		t.Fatalf("full backfill must paint the oldest item")
	}
}

// TestRender_WindowShrinksByRows pins the row budget: a window of few but
// huge items must not paint a freeze-sized buffer either — the item cap
// alone is not enough.
func TestRender_WindowShrinksByRows(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// 30 items of ~100 rows each at 120 cols: 3000 rows over budget.
	for i := 0; i < 30; i++ {
		ui.addAssistantResponse(synthProse(fmt.Sprintf("huge %d", i), 12*1024))
	}
	ui.refreshMessages()

	if ui.windowStart <= 0 {
		t.Fatalf("row budget did not bind: windowStart=%d", ui.windowStart)
	}
	if got := int(ui.paintedLines.Load()); got > renderWindowRows+64 {
		t.Fatalf("painted buffer %d rows exceeds the window budget %d", got, renderWindowRows)
	}
	rendered := ui.Messages.GetText(true)
	if !strings.Contains(rendered, "huge 29") {
		t.Fatalf("window lost the newest item")
	}
	if strings.Contains(rendered, "huge 0") {
		t.Fatalf("row budget did not collapse the oldest item")
	}
}

// TestRender_AppendWorksInsideWindow pins that the incremental append path
// stays available with the window engaged: new tail items append onto the
// windowed buffer instead of forcing a full rebuild.
func TestRender_AppendWorksInsideWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growSession(t, ui, 60)
	ui.refreshMessages()
	fullRenders := ui.fullRenderCount

	ui.appendMessage("a fresh tail prompt")
	if ui.needsFullRender.Load() {
		t.Fatalf("tail append must not force a full rebuild inside the window")
	}
	ui.refreshMessages()
	if ui.fullRenderCount != fullRenders {
		t.Fatalf("tail append took the full-render path (%d rebuilds, want %d)", ui.fullRenderCount, fullRenders)
	}
	if !strings.Contains(ui.Messages.GetText(true), "a fresh tail prompt") {
		t.Fatalf("tail append dropped the new item")
	}
}

// TestCompaction_CollapsesPreCompactionItems pins the view reconciliation
// on compaction (yaah-30t6 AC 3): items below the compaction boundary
// collapse to a one-line stub, later items survive, and the next render
// rebuilds anchors without the compacted prompts and re-windows.
func TestCompaction_CollapsesPreCompactionItems(t *testing.T) {
	ui := newTestUI()
	setupSim(t, ui)

	ui.AddPrompt("first question")
	ui.appendMessage("first answer")
	ui.AddPrompt("second question")
	ui.appendMessage("second answer")
	ui.refreshMessages()
	if len(ui.promptAnchors) != 2 {
		t.Fatalf("expected 2 prompt anchors before compaction, got %d", len(ui.promptAnchors))
	}

	// What CompactionStartedEvent records: the boundary is the log length
	// when compaction begins.
	ui.compactBoundary = 4
	ui.appendMessage("post-boundary note")
	ui.collapseCompacted()

	if got := len(ui.conversationLog); got != 2 {
		t.Fatalf("conversationLog has %d items, want 2 (stub, post-boundary note)", got)
	}
	if !strings.Contains(ui.conversationLog[0].text, "4 messages compacted") {
		t.Fatalf("first item must be the compaction stub: %q", ui.conversationLog[0].text)
	}
	for _, gone := range []string{"first question", "first answer", "second question", "second answer"} {
		if containsItem(ui, gone) {
			t.Fatalf("pre-compaction content must be collapsed: %q", gone)
		}
	}
	if !containsItem(ui, "post-boundary note") {
		t.Fatalf("post-boundary content lost")
	}
	if !ui.needsFullRender.Load() {
		t.Fatalf("collapse must force a full render (the buffer head changed)")
	}

	// The forced render rebuilds anchors without the compacted prompts
	// and re-windows (the log shrank below every budget).
	ui.refreshMessages()
	for _, a := range ui.promptAnchors {
		if a.input == "first question" || a.input == "second question" {
			t.Fatalf("anchor for a compacted prompt survived: %+v", ui.promptAnchors)
		}
	}
	if ui.windowStart != 0 {
		t.Fatalf("window must cover the compacted log, windowStart=%d", ui.windowStart)
	}
}

// TestReconcilePromptPin_AtSeam pins the sticky pin above the window: the
// most recent prompt entirely above the painted window takes over when
// the viewport is at the seam, so the pin survives the "earlier messages"
// collapse.
func TestReconcilePromptPin_AtSeam(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// 130 one-row prompts: the 120-item window starts at prompt 10, so
	// the last pre-window prompt is "q 009".
	for i := 0; i < 130; i++ {
		ui.AddPrompt(fmt.Sprintf("q %03d", i))
	}
	ui.refreshMessages()

	if ui.windowStart != 10 {
		t.Fatalf("windowStart=%d, want 10", ui.windowStart)
	}
	if ui.windowPrePrompt != "q 009" {
		t.Fatalf("windowPrePrompt=%q, want %q", ui.windowPrePrompt, "q 009")
	}

	ui.Messages.ScrollToBeginning()
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "q 009" {
		t.Fatalf("pin at the seam = %q, want the last pre-window prompt %q", ui.pinnedPrompt, "q 009")
	}

	// Scrolling back into the window re-pins the newest prompt at or
	// above the viewport top: one row above q 129's first row pins q 128,
	// exactly on it pins q 129.
	ui.Messages.ScrollTo(ui.promptAnchors[len(ui.promptAnchors)-1].startLine-1, 0)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "q 128" {
		t.Fatalf("pin above the last prompt = %q, want %q", ui.pinnedPrompt, "q 128")
	}
	ui.Messages.ScrollTo(ui.promptAnchors[len(ui.promptAnchors)-1].startLine, 0)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "q 129" {
		t.Fatalf("pin on the last prompt's row = %q, want %q", ui.pinnedPrompt, "q 129")
	}
}

// TestRender_LiveTailRecomputesWindow pins the window budget holding at
// the live tail across a long session: full renders while following the
// tail recompute the trailing window, so the painted range cannot grow
// past the budgets and reintroduce the freeze (Robin F1).
func TestRender_LiveTailRecomputesWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 130; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()
	if ui.windowStart != 10 {
		t.Fatalf("windowStart=%d, want 10", ui.windowStart)
	}

	// The session grows; a full render at the live tail must re-window.
	for i := 130; i < 160; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.needsFullRender.Store(true)
	ui.refreshMessages()
	if ui.windowStart != 40 {
		t.Fatalf("live-tail full render kept the stale window: windowStart=%d, want 40", ui.windowStart)
	}

	// A scrolled-up user keeps their manual window across a full render:
	// the content they are reading must not be yanked away.
	ui.userScrolled = true
	ui.backfillWindow()
	manual := ui.windowStart
	ui.needsFullRender.Store(true)
	ui.refreshMessages()
	if ui.windowStart != manual {
		t.Fatalf("full render moved the manual window: %d -> %d", manual, ui.windowStart)
	}
}

// TestReconcilePromptPin_SeamWithEmptyAnchors pins the pin surviving the
// seam when the painted window contains no prompt items at all: the
// last pre-window prompt takes over (Robin F2).
func TestReconcilePromptPin_SeamWithEmptyAnchors(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	ui.AddPrompt("the only question")
	for i := 0; i < 130; i++ {
		ui.appendMessage(fmt.Sprintf("answer %03d", i))
	}
	ui.refreshMessages()

	if len(ui.promptAnchors) != 0 {
		t.Fatalf("window must contain no prompt anchors, got %d", len(ui.promptAnchors))
	}
	if ui.windowPrePrompt != "the only question" {
		t.Fatalf("windowPrePrompt=%q", ui.windowPrePrompt)
	}
	ui.Messages.ScrollToBeginning()
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "the only question" {
		t.Fatalf("pin at the seam with empty anchors = %q", ui.pinnedPrompt)
	}
}

// TestMaybeBackfillAtSeam pins the shared keyboard/mouse seam trigger:
// the viewport at the top of the painted window backfills, anything
// above it does not (Robin F3).
func TestMaybeBackfillAtSeam(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	ui.Messages.ScrollTo(3, 0)
	ui.maybeBackfillAtSeam()
	if ui.windowStart != 80 {
		t.Fatalf("backfill fired away from the seam: windowStart=%d", ui.windowStart)
	}
	ui.Messages.ScrollTo(0, 0)
	ui.maybeBackfillAtSeam()
	if ui.windowStart != 80-renderBackfillStep {
		t.Fatalf("backfill did not fire at the seam: windowStart=%d", ui.windowStart)
	}
}

// TestSearchMessages_CoversCollapsedItems pins the search spanning the
// whole log: a hit above the painted window expands it and scrolls to
// the match; a miss reports so without touching the window (Robin F4).
func TestSearchMessages_CoversCollapsedItems(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()
	if ui.windowStart != 80 {
		t.Fatalf("windowStart=%d, want 80", ui.windowStart)
	}

	// A collapsed item is found and revealed: the reveal window is
	// bounded — the hit and its context are painted, the tail is not.
	ui.searchMessages("item 005")
	if ui.windowStart > 5 {
		t.Fatalf("search did not expand the window to the hit: windowStart=%d", ui.windowStart)
	}
	if row, _ := ui.Messages.GetScrollOffset(); row <= 0 {
		t.Fatalf("search did not scroll to the hit: offset=%d", row)
	}
	if !strings.Contains(ui.Messages.GetText(true), "item 005") {
		t.Fatalf("hit not painted after reveal")
	}
	if ui.windowEnd != 120 {
		t.Fatalf("reveal window is not bounded: windowEnd=%d, want 120 (item budget)", ui.windowEnd)
	}
	if strings.Contains(ui.Messages.GetText(true), "item 150") {
		t.Fatalf("reveal painted past the window end into the tail")
	}
	if !strings.Contains(ui.Messages.GetText(true), "later messages") {
		t.Fatalf("bottom seam missing from the reveal window")
	}

	// A miss reports and does not move the window.
	before := ui.windowStart
	ui.searchMessages("no such text anywhere")
	if ui.windowStart != before {
		t.Fatalf("miss moved the window: %d -> %d", before, ui.windowStart)
	}
}

// TestReveal_BoundedWindowAtScale pins the round-3 reveal bound (F1): a
// search hit near the top of a large session paints a bounded window
// around it — never the whole transcript — and the tail stays collapsed
// behind the bottom seam.
func TestReveal_BoundedWindowAtScale(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growSession(t, ui, 60)
	ui.refreshMessages()

	before := len(ui.Messages.GetText(true))
	ui.searchMessages("turn 0: investigate")
	if !strings.Contains(ui.Messages.GetText(true), "turn 0: investigate") {
		t.Fatalf("hit not painted after reveal")
	}
	if strings.Contains(ui.Messages.GetText(true), "turn 59: investigate") {
		t.Fatalf("reveal painted the tail: the window is unbounded")
	}
	if ui.windowEnd <= 0 || ui.windowEnd >= len(ui.conversationLog) {
		t.Fatalf("reveal window end = %d, want inside the log", ui.windowEnd)
	}
	if after := len(ui.Messages.GetText(true)); after > before*3 {
		t.Fatalf("reveal repaint blew the budget: %d -> %d bytes", before, after)
	}

	// While the reveal window is pinned, appends must not extend the
	// paint toward the tail.
	for i := 0; i < 5; i++ {
		ui.appendMessage(fmt.Sprintf("tail item %d", i))
	}
	ui.markDirty()
	ui.flushRefresh()
	if strings.Contains(ui.Messages.GetText(true), "tail item 4") {
		t.Fatalf("append grew the reveal window toward the tail")
	}

	// Returning to the tail repaints it.
	ui.returnToTail()
	if ui.windowEnd != -1 {
		t.Fatalf("returnToTail left windowEnd=%d", ui.windowEnd)
	}
	if !strings.Contains(ui.Messages.GetText(true), "tail item 4") {
		t.Fatalf("tail not repainted after returnToTail")
	}
}

// TestReveal_ScrollsToHitRow pins the round-3 row math (F2): the reveal
// repaint makes the painted buffer match the window bookkeeping, so the
// scroll lands on the hit's actual row — including from the reset state
// that used to trust stale bookkeeping.
func TestReveal_ScrollsToHitRow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// The F2 trigger: backfill, return to tail without a repaint, then
	// search — the reveal must still land on the hit's row.
	ui.userScrolled = true
	ui.backfillWindow()
	ui.userScrolled = false
	ui.windowStart = -1
	ui.searchMessages("item 150")

	// The viewport's top row must land on the hit item's row block —
	// the buffer itself is the source of truth, not the row estimate.
	// The reveal scrolls to the item's first row using the itemRows
	// measure (the same one the prompt anchors use), so the content
	// line may sit a few separator rows below the viewport top; T3's
	// exact rows close that drift. The stale-bookkeeping bug this pins
	// scrolled a whole window off.
	row, _ := ui.Messages.GetScrollOffset()
	lines := strings.Split(ui.Messages.GetText(true), "\n")
	hit := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "item 150" {
			hit = i
			break
		}
	}
	if hit < 0 {
		t.Fatalf("hit not painted after reveal")
	}
	w := messageWidth(ui.Messages)
	block := ui.itemRows(&ui.conversationLog[150], ui.buildItems(150, 151, w)[0], w)
	if row > hit || hit-row >= block {
		t.Fatalf("reveal scrolled to row %d; the hit at row %d is not at the top of its block", row, hit)
	}
}

// TestRevealItem_NormalizesUnestablishedWindow pins the round-2 reveal fix:
// after a live-tail reset (windowStart -1, no full render since), revealing
// a hit must normalize the window first — a painted-range hit used to start
// the row loop at -1 and index conversationLog[-1], and a collapsed hit
// stayed unpainted (Robin F1).
func TestRevealItem_NormalizesUnestablishedWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// Painted-range hit from the reset state: must not panic.
	ui.windowStart = -1
	ui.revealItem(199)
	if ui.windowStart < 0 {
		t.Fatalf("reveal left the window unestablished: %d", ui.windowStart)
	}

	// Collapsed hit from the reset state: must expand and paint.
	ui.windowStart = -1
	ui.revealItem(5)
	if ui.windowStart > 5 {
		t.Fatalf("reveal did not expand the window to the hit: %d", ui.windowStart)
	}
	if !strings.Contains(ui.Messages.GetText(true), "item 005") {
		t.Fatalf("hit not painted after reveal")
	}
}

// TestSearchMarkdownLinkLabel pins the round-2 search-text fix: markdown
// items are searched raw, so a link label inside [label](url) stays
// findable — StripTags eats the bracket group and hid the label (Robin F2).
func TestSearchMarkdownLinkLabel(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	ui.addAssistantResponse("see [the docs](https://example.com) for details")
	ui.refreshMessages()

	want := "see [the docs](https://example.com) for details"
	if got := ui.searchableItemText(&ui.conversationLog[0]); got != want {
		t.Fatalf("markdown search text = %q, want %q", got, want)
	}
	ui.searchMessages("the docs")
	if ui.ephemeralMsg != "Found in message 1 of 1" {
		t.Fatalf("link label not found: ephemeral = %q", ui.ephemeralMsg)
	}
}

// TestPromptIndex_MaintainedAcrossRewrites pins the round-2 anchor fix:
// promptIdxs tracks prompt items exactly across appends, the compaction
// rewrite, and clear, and rebuildPromptAnchors derives windowPrePrompt
// from it by binary search instead of a prefix scan (Robin F3).
func TestPromptIndex_MaintainedAcrossRewrites(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	ui.AddPrompt("first question")
	ui.appendMessage("answer one")
	ui.AddPrompt("second question")
	ui.appendMessage("answer two")

	if got, want := ui.promptIdxs, []int{0, 2}; !slices.Equal(got, want) {
		t.Fatalf("promptIdxs = %v, want %v", got, want)
	}

	// A window starting past the first prompt pins it above the seam;
	// the second prompt is inside the painted range.
	items := ui.buildItems(2, 4, 120)
	ui.rebuildPromptAnchors(items, 120, 2)
	if ui.windowPrePrompt != "first question" {
		t.Fatalf("windowPrePrompt = %q", ui.windowPrePrompt)
	}

	// Compaction rewrites the log; the index list is rebuilt to match.
	ui.compactBoundary = 2
	ui.collapseCompacted()
	if got, want := ui.promptIdxs, []int{1}; !slices.Equal(got, want) {
		t.Fatalf("promptIdxs after compaction = %v, want %v", got, want)
	}

	// Clearing the conversation clears the index.
	ui.clearConversation()
	if len(ui.promptIdxs) != 0 {
		t.Fatalf("promptIdxs survived clear: %v", ui.promptIdxs)
	}
}

// TestCmdBottom_ResetsLiveTail pins the round-2 :bottom fix: the command
// mirrors the mouse scroll-down live-tail reset (userScrolled cleared,
// window re-established at the next full render) instead of leaving the
// view in manual-scroll mode (Robin F5).
func TestCmdBottom_ResetsLiveTail(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 20; i++ {
		ui.appendMessage(fmt.Sprintf("item %02d", i))
	}
	ui.refreshMessages()

	ui.userScrolled = true
	ui.windowStart = 5
	ui.windowWidth = 120
	if !ui.HandleCommand(command.CmdBottom, "") {
		t.Fatalf("CmdBottom was not handled")
	}
	if ui.userScrolled {
		t.Fatalf("CmdBottom left the view in manual-scroll mode")
	}
	if ui.windowStart != -1 {
		t.Fatalf("CmdBottom kept a stale window: %d", ui.windowStart)
	}
}

// TestCmdTop_BackfillsFromAnyRow pins the round-3 :top fix: the command
// backfills unconditionally while older items are collapsed — the seam-row
// gate used to leave ":top" from mid-scroll landing on the seam line
// instead of readable content.
func TestCmdTop_BackfillsFromAnyRow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()
	if ui.windowStart != 80 {
		t.Fatalf("windowStart=%d, want 80", ui.windowStart)
	}

	// Away from row 0: :top must still load the next batch.
	ui.Messages.ScrollTo(5, 0)
	if !ui.HandleCommand(command.CmdTop, "") {
		t.Fatalf("CmdTop was not handled")
	}
	if ui.windowStart != 80-renderBackfillStep {
		t.Fatalf(":top away from the seam did not backfill: windowStart=%d", ui.windowStart)
	}
}

// TestBackfill_BatchCappedByRows pins the round-3 batch bound: a
// backfill prepends at most renderBackfillStep items AND at most
// renderWindowRows rows — a batch of tall tool or markdown items must
// not repaint a freeze-sized buffer on the lazy scroll path.
func TestBackfill_BatchCappedByRows(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 20; i++ {
		ui.appendMessage(fmt.Sprintf("item %02d\n%s", i, strings.Repeat("row\n", 400)))
	}
	ui.refreshMessages()
	if ui.windowStart <= 0 {
		t.Fatalf("row budget did not engage: windowStart=%d", ui.windowStart)
	}
	oldStart := ui.windowStart

	ui.userScrolled = true // callers set it before backfilling
	ui.backfillWindow()
	batch := oldStart - ui.windowStart
	if batch <= 0 {
		t.Fatalf("backfill made no progress: windowStart=%d -> %d", oldStart, ui.windowStart)
	}
	if batch >= renderBackfillStep {
		t.Fatalf("row cap did not bind: batch=%d items, want < %d", batch, renderBackfillStep)
	}
}

// TestBackfill_KeepsManualWindowOnWidthChange pins the round-3 width fix:
// backfillWindow records the current width so a resize between renders
// cannot make the repaint recompute the trailing window and yank the
// user to the live tail mid-backfill.
func TestBackfill_KeepsManualWindowOnWidthChange(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	ui.userScrolled = true // callers set it before backfilling
	ui.windowWidth = 42    // stale: the pane was resized since the window
	ui.backfillWindow()
	if ui.windowStart != 80-renderBackfillStep {
		t.Fatalf("stale width re-windowed the backfill: windowStart=%d", ui.windowStart)
	}
	if ui.windowWidth != messageWidth(ui.Messages) {
		t.Fatalf("backfill left a stale windowWidth=%d, want %d", ui.windowWidth, messageWidth(ui.Messages))
	}
}

func containsItem(ui *App, want string) bool {
	for i := range ui.conversationLog {
		if strings.Contains(ui.conversationLog[i].text, want) {
			return true
		}
	}
	return false
}
