package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/components/command"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// TestRender_WindowBoundsPaint pins the windowed painter at session scale:
// a full render paints the trailing window (last WindowItems, row-budgeted),
// collapses everything older behind the seam, and the painted rows no
// longer contain the early turns.
func TestRender_WindowBoundsPaint(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// ~4-5 items per turn: 60 turns overflows the 120-item window.
	growSession(t, ui, 60)

	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws <= 0 {
		t.Fatalf("window did not engage: windowStart=%d, log=%d items", ws, ui.tr.Len())
	}
	rendered := ui.tr.PaintedText()
	if !strings.Contains(rendered, fmt.Sprintf("turn %d: investigate", 59)) {
		t.Fatalf("window lost the newest turn")
	}
	if strings.Contains(rendered, "turn 0: investigate") {
		t.Fatalf("window painted the oldest turn: the seam did not collapse it")
	}
	if !strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam line missing from the painted rows")
	}
}

// TestRender_BackfillSlidesWindow pins the lazy backfill: prepending
// BackfillStep older items paints them, keeps the seam until the window
// covers the whole log, and shifts the viewport so the content the user
// was looking at stays put.
func TestRender_BackfillSlidesWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// 200 small distinctive items: the item cap binds (rows stay well
	// under budget), so the window is exactly the last 120 items.
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	if ws := ui.tr.WindowStart(); ws != 80 {
		t.Fatalf("windowStart=%d, want 80 (200 items, 120-item window)", ws)
	}
	rendered := ui.tr.PaintedText()
	if !strings.Contains(rendered, "item 199") || !strings.Contains(rendered, "item 080") {
		t.Fatalf("window lost the newest items")
	}
	if strings.Contains(rendered, "item 079") {
		t.Fatalf("items above the window are painted")
	}

	// The user is at the seam (viewport offset 0); backfill one step.
	ui.tr.SetScrolled(true)
	ui.backfill()

	if ws := ui.tr.WindowStart(); ws != 80-transcript.BackfillStep {
		t.Fatalf("backfill moved windowStart 80 -> %d, want %d", ws, 80-transcript.BackfillStep)
	}
	rendered = ui.tr.PaintedText()
	if !strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam must persist while older items remain")
	}
	if !strings.Contains(rendered, "item 068") {
		t.Fatalf("backfill did not paint the newly loaded items")
	}
	// Viewport stability: the offset advanced past the seam so the row
	// the user was looking at stays put.
	if row := ui.pane.Offset(); row <= 0 {
		t.Fatalf("backfill left the viewport at the seam: offset=%d", row)
	}

	// Backfilling to the top removes the seam and paints item 000.
	for ui.tr.WindowStart() > 0 {
		prev := ui.tr.WindowStart()
		ui.backfill()
		if ws := ui.tr.WindowStart(); ws >= prev {
			t.Fatalf("backfill stalled at %d", prev)
		}
	}
	rendered = ui.tr.PaintedText()
	if strings.Contains(rendered, "earlier messages") {
		t.Fatalf("seam must vanish once the window covers the whole log")
	}
	if !strings.Contains(rendered, "item 000") {
		t.Fatalf("full backfill must paint the oldest item")
	}
}

// TestRender_WindowShrinksByRows pins the row budget: a window of few but
// huge items must not paint a freeze-sized row table either — the item cap
// alone is not enough.
func TestRender_WindowShrinksByRows(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	// 30 items of ~100 rows each at 120 cols: 3000 rows over budget.
	for i := 0; i < 30; i++ {
		ui.addAssistantResponse(synthProse(fmt.Sprintf("huge %d", i), 12*1024))
	}
	ui.refreshMessages()

	if ws := ui.tr.WindowStart(); ws <= 0 {
		t.Fatalf("row budget did not bind: windowStart=%d", ws)
	}
	if got := ui.tr.PaintedLines(); got > transcript.WindowRows+64 {
		t.Fatalf("painted rows %d exceeds the window budget %d", got, transcript.WindowRows)
	}
	rendered := ui.tr.PaintedText()
	if !strings.Contains(rendered, "huge 29") {
		t.Fatalf("window lost the newest item")
	}
	if strings.Contains(rendered, "huge 0") {
		t.Fatalf("row budget did not collapse the oldest item")
	}
}

// TestRender_AppendWorksInsideWindow pins that the incremental append path
// stays available with the window engaged: new tail items append onto the
// windowed rows instead of forcing a full rebuild.
func TestRender_AppendWorksInsideWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	growSession(t, ui, 60)
	ui.refreshMessages()
	fullRenders := ui.tr.FullRenders()

	ui.appendMessage("a fresh tail prompt")
	ui.refreshMessages()
	if got := ui.tr.FullRenders(); got != fullRenders {
		t.Fatalf("tail append took the full-render path (%d rebuilds, want %d)", got, fullRenders)
	}
	if !strings.Contains(ui.tr.PaintedText(), "a fresh tail prompt") {
		t.Fatalf("tail append dropped the new item")
	}
}

// TestCompaction_CollapsesPreCompactionItems pins the view reconciliation
// on compaction (yaah-30t6 AC 3): items below the compaction boundary
// collapse to a one-line stub, later items survive, and the next repaint
// rebuilds anchors without the compacted prompts and re-windows.
func TestCompaction_CollapsesPreCompactionItems(t *testing.T) {
	ui := newTestUI()
	setupSim(t, ui)

	ui.AddPrompt("first question")
	ui.appendMessage("first answer")
	ui.AddPrompt("second question")
	ui.appendMessage("second answer")
	ui.refreshMessages()
	if len(ui.tr.Anchors()) != 2 {
		t.Fatalf("expected 2 prompt anchors before compaction, got %d", len(ui.tr.Anchors()))
	}

	// What CompactionStartedEvent records: the boundary is the log length
	// when compaction begins.
	ui.compactBoundary = 4
	ui.appendMessage("post-boundary note")
	ui.collapseCompacted()

	if got := ui.tr.Len(); got != 2 {
		t.Fatalf("transcript has %d items, want 2 (stub, post-boundary note)", got)
	}
	if !strings.Contains(ui.tr.ItemAt(0).Text, "4 messages compacted") {
		t.Fatalf("first item must be the compaction stub: %q", ui.tr.ItemAt(0).Text)
	}
	for _, gone := range []string{"first question", "first answer", "second question", "second answer"} {
		if containsItem(ui, gone) {
			t.Fatalf("pre-compaction content must be collapsed: %q", gone)
		}
	}
	if !containsItem(ui, "post-boundary note") {
		t.Fatalf("post-boundary content lost")
	}

	// The repaint rebuilds anchors without the compacted prompts and
	// re-windows (the log shrank below every budget).
	ui.refreshMessages()
	for _, a := range ui.tr.Anchors() {
		if a.Text == "first question" || a.Text == "second question" {
			t.Fatalf("anchor for a compacted prompt survived: %+v", ui.tr.Anchors())
		}
	}
	if ws := ui.tr.WindowStart(); ws != 0 {
		t.Fatalf("window must cover the compacted log, windowStart=%d", ws)
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

	if ws := ui.tr.WindowStart(); ws != 10 {
		t.Fatalf("windowStart=%d, want 10", ws)
	}
	if got := ui.tr.PreWindowPrompt(); got != "q 009" {
		t.Fatalf("preWindowPrompt=%q, want %q", got, "q 009")
	}

	ui.pane.ScrollToBeginning()
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "q 009" {
		t.Fatalf("pin at the seam = %q, want the last pre-window prompt %q", ui.pinnedPrompt, "q 009")
	}

	// Scrolling back into the window re-pins the newest prompt whose
	// first row has scrolled past the viewport top (yaah-983 hysteresis).
	// The checks ride a mid-window anchor: the pane clamps the offset to
	// the row table, so the last viewport's worth of prompts can never
	// sit at the viewport top.
	anchors := ui.tr.Anchors()
	i := len(anchors) / 2
	a, prev := anchors[i], anchors[i-1].Text
	ui.pane.ScrollTo(a.Row - 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != prev {
		t.Fatalf("pin above %s = %q, want %q", a.Text, ui.pinnedPrompt, prev)
	}
	ui.pane.ScrollTo(a.Row)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != prev {
		t.Fatalf("pin on %s's row = %q, want %q (hysteresis: in-stream copy is visible)", a.Text, ui.pinnedPrompt, prev)
	}
	ui.pane.ScrollTo(a.Row + 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != a.Text {
		t.Fatalf("pin past %s's row = %q, want %q", a.Text, ui.pinnedPrompt, a.Text)
	}
}

// TestRender_LiveTailRecomputesWindow pins the window budget holding at
// the live tail across a long session: full repaints while following the
// tail recompute the trailing window, so the painted range cannot grow
// past the budgets and reintroduce the freeze (Robin F1).
func TestRender_LiveTailRecomputesWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 130; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws != 10 {
		t.Fatalf("windowStart=%d, want 10", ws)
	}

	// The session grows; a full repaint at the live tail must re-window.
	for i := 130; i < 160; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.tr.RequestFull()
	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws != 40 {
		t.Fatalf("live-tail full render kept the stale window: windowStart=%d, want 40", ws)
	}

	// A scrolled-up user keeps their manual window across a full
	// repaint: the content they are reading must not be yanked away.
	ui.tr.SetScrolled(true)
	ui.backfill()
	manual := ui.tr.WindowStart()
	ui.tr.RequestFull()
	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws != manual {
		t.Fatalf("full render moved the manual window: %d -> %d", manual, ws)
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

	if n := len(ui.tr.Anchors()); n != 0 {
		t.Fatalf("window must contain no prompt anchors, got %d", n)
	}
	if got := ui.tr.PreWindowPrompt(); got != "the only question" {
		t.Fatalf("preWindowPrompt=%q", got)
	}
	ui.pane.ScrollToBeginning()
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "the only question" {
		t.Fatalf("pin at the seam with empty anchors = %q", ui.pinnedPrompt)
	}
}

// TestMaybeBackfillAtSeam pins the shared keyboard/mouse seam trigger:
// the viewport at the top of the painted window backfills, anything
// below it does not (Robin F3).
func TestMaybeBackfillAtSeam(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	ui.pane.ScrollTo(3)
	ui.maybeBackfillAtSeam()
	if ws := ui.tr.WindowStart(); ws != 80 {
		t.Fatalf("backfill fired away from the seam: windowStart=%d", ws)
	}
	ui.pane.ScrollTo(0)
	ui.maybeBackfillAtSeam()
	if ws := ui.tr.WindowStart(); ws != 80-transcript.BackfillStep {
		t.Fatalf("backfill did not fire at the seam: windowStart=%d", ws)
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
	if ws := ui.tr.WindowStart(); ws != 80 {
		t.Fatalf("windowStart=%d, want 80", ws)
	}

	// A collapsed item is found and revealed: the reveal window is
	// bounded — the hit and its context are painted, the tail is not.
	ui.searchMessages("item 005")
	if ws := ui.tr.WindowStart(); ws > 5 {
		t.Fatalf("search did not expand the window to the hit: windowStart=%d", ws)
	}
	if row := ui.pane.Offset(); row <= 0 {
		t.Fatalf("search did not scroll to the hit: offset=%d", row)
	}
	if !strings.Contains(ui.tr.PaintedText(), "item 005") {
		t.Fatalf("hit not painted after reveal")
	}
	if we := ui.tr.WindowEnd(); we != 120 {
		t.Fatalf("reveal window is not bounded: windowEnd=%d, want 120 (item budget)", we)
	}
	if strings.Contains(ui.tr.PaintedText(), "item 150") {
		t.Fatalf("reveal painted past the window end into the tail")
	}
	if !strings.Contains(ui.tr.PaintedText(), "later messages") {
		t.Fatalf("bottom seam missing from the reveal window")
	}

	// A miss reports and does not move the window.
	before := ui.tr.WindowStart()
	ui.searchMessages("no such text anywhere")
	if ws := ui.tr.WindowStart(); ws != before {
		t.Fatalf("miss moved the window: %d -> %d", before, ws)
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

	before := len(ui.tr.PaintedText())
	ui.searchMessages("turn 0: investigate")
	if !strings.Contains(ui.tr.PaintedText(), "turn 0: investigate") {
		t.Fatalf("hit not painted after reveal")
	}
	if strings.Contains(ui.tr.PaintedText(), "turn 59: investigate") {
		t.Fatalf("reveal painted the tail: the window is unbounded")
	}
	if we := ui.tr.WindowEnd(); we <= 0 || we >= ui.tr.Len() {
		t.Fatalf("reveal window end = %d, want inside the log", we)
	}
	if after := len(ui.tr.PaintedText()); after > before*3 {
		t.Fatalf("reveal repaint blew the budget: %d -> %d bytes", before, after)
	}

	// While the reveal window is pinned, appends must not extend the
	// paint toward the tail.
	for i := 0; i < 5; i++ {
		ui.appendMessage(fmt.Sprintf("tail item %d", i))
	}
	ui.markDirty()
	ui.flushRefresh()
	if strings.Contains(ui.tr.PaintedText(), "tail item 4") {
		t.Fatalf("append grew the reveal window toward the tail")
	}

	// Returning to the tail repaints it.
	ui.returnToTail()
	if we := ui.tr.WindowEnd(); we != -1 {
		t.Fatalf("returnToTail left windowEnd=%d", we)
	}
	if !strings.Contains(ui.tr.PaintedText(), "tail item 4") {
		t.Fatalf("tail not repainted after returnToTail")
	}
}

// TestReveal_ScrollsToHitRow pins the exact reveal row math: the prefix
// table maps the hit to its exact first painted row, so the viewport
// lands with the hit's block at the top — including from the reset
// state that used to trust stale bookkeeping (round-3 F2).
func TestReveal_ScrollsToHitRow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// The F2 trigger: backfill, return to tail without a repaint, then
	// search — the reveal must still land on the hit's row.
	ui.tr.SetScrolled(true)
	ui.backfill()
	ui.tr.ReturnToTail()
	ui.searchMessages("item 150")

	// The viewport's top row must land exactly on the hit's first row,
	// and the hit's content must sit inside the first few rows of the
	// block (a leading separator row may precede it).
	row := ui.pane.Offset()
	want, ok := ui.tr.ItemRow(150)
	if !ok {
		t.Fatalf("hit is not inside the reveal window")
	}
	if row != want {
		t.Fatalf("reveal scrolled to row %d; the hit's exact first row is %d", row, want)
	}
	hit := -1
	for i, ln := range ui.tr.Rows() {
		if strings.TrimSpace(ln) == "item 150" {
			hit = i
			break
		}
	}
	if hit < 0 {
		t.Fatalf("hit not painted after reveal")
	}
	if hit < row || hit-row > 3 {
		t.Fatalf("hit content row %d not at the top of its block (viewport %d)", hit, row)
	}
}

// TestRevealItem_NormalizesUnestablishedWindow pins the round-2 reveal fix:
// after a live-tail reset (window unestablished, no repaint since),
// revealing a hit must normalize the window first — a painted-range hit
// used to index before the log's start, and a collapsed hit stayed
// unpainted (Robin F1).
func TestRevealItem_NormalizesUnestablishedWindow(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// Painted-range hit from the reset state: must not panic.
	ui.tr.ReturnToTail() // the live-tail reset without a repaint
	ui.revealItem(199)
	if ws := ui.tr.WindowStart(); ws < 0 {
		t.Fatalf("reveal left the window unestablished: %d", ws)
	}

	// Collapsed hit from the reset state: must expand and paint.
	ui.tr.ReturnToTail()
	ui.revealItem(5)
	if ws := ui.tr.WindowStart(); ws > 5 {
		t.Fatalf("reveal did not expand the window to the hit: %d", ws)
	}
	if !strings.Contains(ui.tr.PaintedText(), "item 005") {
		t.Fatalf("hit not painted after reveal")
	}
}

// TestSearchMarkdownLinkLabel pins the round-2 search-text fix: markdown
// items are searched raw, so a link label inside [label](url) stays
// findable — tag stripping eats the bracket group and hid the label
// (Robin F2).
func TestSearchMarkdownLinkLabel(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	ui.addAssistantResponse("see [the docs](https://example.com) for details")
	ui.refreshMessages()

	want := "see [the docs](https://example.com) for details"
	if got, _ := ui.tr.SearchItem(0, "the docs"); got != want {
		t.Fatalf("markdown search text = %q, want %q", got, want)
	}
	ui.searchMessages("the docs")
	if ui.ephemeralMsg != "Found in message 1 of 1" {
		t.Fatalf("link label not found: ephemeral = %q", ui.ephemeralMsg)
	}
}

// TestPromptIndex_MaintainedAcrossRewrites pins the round-2 anchor fix:
// the model's prompt index tracks prompt items exactly across appends,
// the compaction rewrite, and clear, and the window rebuild derives
// the pre-window prompt from it by binary search instead of a prefix
// scan (Robin F3).
func TestPromptIndex_MaintainedAcrossRewrites(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	ui.AddPrompt("first question")
	ui.appendMessage("answer one")
	ui.AddPrompt("second question")
	ui.appendMessage("answer two")

	if got, want := ui.tr.PromptIndices(), []int{0, 2}; !slices.Equal(got, want) {
		t.Fatalf("promptIndices = %v, want %v", got, want)
	}

	// A window starting past both prompts pins the second above the
	// seam: grow the log until the trailing window leaves the prompts
	// behind, then repaint.
	for i := 0; i < 130; i++ {
		ui.appendMessage(fmt.Sprintf("filler %03d", i))
	}
	ui.tr.RequestFull()
	ui.refreshMessages()
	if got := ui.tr.PreWindowPrompt(); got != "second question" {
		t.Fatalf("preWindowPrompt = %q, want %q", got, "second question")
	}

	// Compaction rewrites the log; the index list is rebuilt to match.
	ui.compactBoundary = 2
	ui.collapseCompacted()
	if got, want := ui.tr.PromptIndices(), []int{1}; !slices.Equal(got, want) {
		t.Fatalf("promptIndices after compaction = %v, want %v", got, want)
	}

	// Clearing the conversation clears the index.
	ui.clearConversation()
	if n := len(ui.tr.PromptIndices()); n != 0 {
		t.Fatalf("promptIndices survived clear: %v", ui.tr.PromptIndices())
	}
}

// TestCmdBottom_ResetsLiveTail pins the round-2 :bottom fix: the command
// mirrors the mouse scroll-down live-tail reset (userScrolled cleared,
// window re-established at the next full repaint) instead of leaving the
// view in manual-scroll mode (Robin F5).
func TestCmdBottom_ResetsLiveTail(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// Install a manual window the way a scroll-up does.
	ui.tr.SetScrolled(true)
	ui.backfill()
	if ui.tr.WindowStart() == 80 {
		t.Fatalf("precondition: backfill did not move the window")
	}

	if !ui.HandleCommand(command.CmdBottom, "") {
		t.Fatalf("CmdBottom was not handled")
	}
	if ui.tr.UserScrolled() {
		t.Fatalf("CmdBottom left the view in manual-scroll mode")
	}
	if ws := ui.tr.WindowStart(); ws != -1 {
		t.Fatalf("CmdBottom kept a stale window: %d", ws)
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
	if ws := ui.tr.WindowStart(); ws != 80 {
		t.Fatalf("windowStart=%d, want 80", ws)
	}

	// Away from row 0: :top must still load the next batch.
	ui.pane.ScrollTo(5)
	if !ui.HandleCommand(command.CmdTop, "") {
		t.Fatalf("CmdTop was not handled")
	}
	if ws := ui.tr.WindowStart(); ws != 80-transcript.BackfillStep {
		t.Fatalf(":top away from the seam did not backfill: windowStart=%d", ws)
	}
}

// TestBackfill_BatchCappedByRows pins the round-3 batch bound: a
// backfill prepends at most BackfillStep items AND at most WindowRows
// rows — a batch of tall tool or markdown items must not repaint a
// freeze-sized row table on the lazy scroll path.
func TestBackfill_BatchCappedByRows(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 20; i++ {
		ui.appendMessage(fmt.Sprintf("item %02d\n%s", i, strings.Repeat("row\n", 400)))
	}
	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws <= 0 {
		t.Fatalf("row budget did not engage: windowStart=%d", ws)
	}
	oldStart := ui.tr.WindowStart()

	ui.tr.SetScrolled(true) // callers set it before backfilling
	ui.backfill()
	batch := oldStart - ui.tr.WindowStart()
	if batch <= 0 {
		t.Fatalf("backfill made no progress: windowStart=%d -> %d", oldStart, ui.tr.WindowStart())
	}
	if batch >= transcript.BackfillStep {
		t.Fatalf("row cap did not bind: batch=%d items, want < %d", batch, transcript.BackfillStep)
	}
}

// TestBackfill_KeepsManualWindowOnWidthChange pins the round-3 width fix:
// backfill records the current width so a resize between repaints cannot
// make the next full repaint recompute the trailing window and yank the
// user to the live tail mid-backfill.
func TestBackfill_KeepsManualWindowOnWidthChange(t *testing.T) {
	ui := gateApp()
	sim := setupSim(t, ui)
	for i := 0; i < 200; i++ {
		ui.appendMessage(fmt.Sprintf("item %03d", i))
	}
	ui.refreshMessages()

	// Resize the pane after the window was computed at the old width,
	// then backfill: the manual window must survive (the model re-records
	// the width itself), and a forced full repaint afterwards must keep
	// it instead of recomputing the trailing window.
	sim.SetSize(100, 40)
	ui.Root.SetRect(0, 0, 100, 40)
	ui.Root.Draw(sim)
	ui.tr.SetScrolled(true)
	ui.backfill()
	if ws := ui.tr.WindowStart(); ws != 80-transcript.BackfillStep {
		t.Fatalf("resize re-windowed the backfill: windowStart=%d", ws)
	}
	manual := ui.tr.WindowStart()
	ui.tr.RequestFull()
	ui.refreshMessages()
	if ws := ui.tr.WindowStart(); ws != manual {
		t.Fatalf("full repaint moved the manual window mid-backfill: %d -> %d", manual, ws)
	}
}

func containsItem(ui *App, want string) bool {
	for i := 0; i < ui.tr.Len(); i++ {
		if strings.Contains(ui.tr.ItemAt(i).Text, want) {
			return true
		}
	}
	return false
}

// TestReveal_WindowContainsHitBehindHugeItem pins the reveal budget rule:
// an item taller than the whole row budget directly above the search hit
// used to stop the forward window walk before the hit, so ItemRow
// reported its out-of-range sentinel and the reveal scrolled to the top.
// The hit must always sit inside its own reveal window.
func TestReveal_WindowContainsHitBehindHugeItem(t *testing.T) {
	ui := gateApp()
	setupSim(t, ui)
	for i := 0; i < 40; i++ {
		ui.appendMessage(fmt.Sprintf("item %02d", i))
	}
	// One item taller than the whole row budget immediately above the
	// hit, and tail items after it so the reveal window is bounded.
	ui.appendMessage(strings.Repeat("huge line\n", transcript.WindowRows+50))
	ui.appendMessage("the needle")
	for i := 0; i < 10; i++ {
		ui.appendMessage(fmt.Sprintf("tail %02d", i))
	}
	hit := ui.tr.Len() - 11
	ui.refreshMessages()

	ui.revealItem(hit)

	// The old bug: the forward walk stopped at the huge item, the hit
	// fell outside the window, and ItemRow's sentinel scrolled to the
	// top. The hit must now be inside the window and on screen.
	row, ok := ui.tr.ItemRow(hit)
	if !ok {
		t.Fatalf("hit %d is not inside its reveal window", hit)
	}
	found := false
	for _, ln := range ui.tr.Rows() {
		if strings.Contains(ln, "the needle") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("hit is not painted after reveal")
	}
	_, _, _, h := ui.pane.GetInnerRect()
	if off := ui.pane.Offset(); row < off || row >= off+h {
		t.Fatalf("hit row %d is not visible: viewport rows %d-%d", row, off, off+h)
	}
}
