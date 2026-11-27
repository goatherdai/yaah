package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// TestAddPrompt_RendersInStream_DoesNotPin verifies the submit-path
// change: the question renders in the conversation stream, and the
// sticky header stays empty — the pin is scroll-derived, not submit-
// derived.
func TestAddPrompt_RendersInStream_DoesNotPin(t *testing.T) {
	ui := New("test")

	ui.AddPrompt("what about the swap path?")
	if ui.pinnedPrompt != "" {
		t.Fatalf("AddPrompt must not pin the prompt, got %q", ui.pinnedPrompt)
	}
	ui.refreshMessages()

	buf := ui.tr.PaintedText()
	if !strings.Contains(buf, "🍖") || !strings.Contains(buf, "what about the swap path?") {
		t.Fatalf("prompt missing from conversation stream: %q", buf)
	}
	if ui.pinnedPrompt != "" || ui.promptEcho.GetText(true) != "" {
		t.Fatalf("prompt pinned before crossing the seam: pin=%q echo=%q",
			ui.pinnedPrompt, ui.promptEcho.GetText(true))
	}
	anchors := ui.tr.Anchors()
	if len(anchors) != 1 || anchors[0].Text != "what about the swap path?" {
		t.Fatalf("anchor table wrong: %+v", anchors)
	}
}

// TestEchoPrompt_FlushesStreamedTextFirst pins the steer/follow-up echo
// ordering: pending streamed tokens must land in the conversation before
// the echoed prompt, so a mid-stream steer does not render the user's
// question ahead of the assistant text it interrupted.
func TestEchoPrompt_FlushesStreamedTextFirst(t *testing.T) {
	ui := New("test")

	// Simulate a mid-stream steer: tokens are buffered and the queued
	// flush callback has not run yet.
	ui.isStreaming.Store(true)
	ui.tokenMu.Lock()
	ui.pendingTokens.WriteString("partial answer")
	ui.tokenMu.Unlock()

	ui.EchoPrompt("steer: keep it short")

	if got := ui.tr.Len(); got != 2 {
		t.Fatalf("transcript has %d items, want 2 (assistant text, prompt)", got)
	}
	first, second := ui.tr.ItemAt(0), ui.tr.ItemAt(1)
	if !first.Markdown || !strings.Contains(first.Text, "partial answer") {
		t.Fatalf("first item must be the flushed assistant text: %+v", first)
	}
	if !second.IsPrompt || second.Prompt != "steer: keep it short" {
		t.Fatalf("second item must be the echoed prompt: %+v", second)
	}
	if ui.isStreaming.Load() {
		t.Fatal("flush must clear the streaming flag")
	}
}

// TestReconcilePromptPin_Rules drives the scroll position through the
// full lifecycle: no pin at the transcript top, pin once a prompt's
// first row has scrolled PAST the viewport top (yaah-983 hysteresis: at
// the exact boundary row the in-stream copy is visible, so the pin
// shows the previous prompt instead of double-rendering), swap to the
// newer prompt when it crosses, reverse scroll re-pins the previous one.
func TestReconcilePromptPin_Rules(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	sim.SetSize(120, 24)
	ui := New("test")
	ui.Root.SetRect(0, 0, 120, 24)
	ui.Root.Draw(sim)

	ui.AddPrompt("question one")
	// Taller than the viewport, so the scroll positions below exist:
	// the pane clamps the offset to the row table (a real viewport cannot
	// scroll into nothing), unlike the TextView it replaces.
	ui.appendMessage(strings.Repeat("filler line\n", 40))
	ui.refreshMessages()
	if len(ui.tr.Anchors()) != 1 {
		t.Fatalf("expected one anchor, got %+v", ui.tr.Anchors())
	}
	q1Row := ui.tr.Anchors()[0].Row

	// At the transcript top: unpinned.
	ui.pane.ScrollTo(0)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "" {
		t.Fatalf("prompt pinned at the top: %q", ui.pinnedPrompt)
	}

	// The prompt's first row exactly at the viewport top: the in-stream
	// copy is on screen, so the pin stays empty (hysteresis, yaah-983).
	ui.pane.ScrollTo(q1Row)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "" {
		t.Fatalf("prompt pinned while its in-stream copy is at the top row: %q", ui.pinnedPrompt)
	}

	// One row past it: the in-stream copy has scrolled off, the pin
	// takes over. The echo shows the question text and the stream still
	// holds its own copy.
	ui.pane.ScrollTo(q1Row + 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "question one" {
		t.Fatalf("seam crossing did not pin; got %q", ui.pinnedPrompt)
	}
	if !strings.Contains(ui.promptEcho.GetText(true), "question one") {
		t.Fatalf("echo missing pinned prompt: %q", ui.promptEcho.GetText(true))
	}
	if !strings.Contains(ui.tr.PaintedText(), "question one") {
		t.Fatalf("in-stream copy lost: %q", ui.tr.PaintedText())
	}

	// A second question, still below the seam: the pin holds q1. The
	// trailing filler keeps q2's boundary inside the scrollable range
	// (the pane clamps the offset a viewport short of the end).
	ui.AddPrompt("question two")
	ui.AddToolStart("1", "bash", "ls") // grows the row table via the fast path
	ui.appendMessage(strings.Repeat("filler line\n", 40))
	ui.refreshMessages()
	anchors := ui.tr.Anchors()
	if len(anchors) != 2 {
		t.Fatalf("expected two anchors, got %+v", anchors)
	}
	q2 := anchors[1]
	if q2.Row <= q1Row {
		t.Fatalf("q2 anchor not below q1: %+v", anchors)
	}

	// Scroll still near q1: the pin holds q1.
	ui.pane.ScrollTo(q1Row + 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "question one" {
		t.Fatalf("second question took the pin before crossing the seam: %q", ui.pinnedPrompt)
	}

	// q2's first row at the viewport top: still showing q1 (hysteresis).
	ui.pane.ScrollTo(q2.Row)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "question one" {
		t.Fatalf("q2 took the pin while its in-stream copy is at the top row: %q", ui.pinnedPrompt)
	}

	// One row past q2: swap.
	ui.pane.ScrollTo(q2.Row + 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "question two" {
		t.Fatalf("q2 did not take over the pin: %q", ui.pinnedPrompt)
	}

	// Reverse scroll above q2's boundary: q2 detaches, q1 re-pins.
	ui.pane.ScrollTo(q1Row + 1)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "question one" {
		t.Fatalf("reverse scroll did not re-pin q1: %q", ui.pinnedPrompt)
	}

	// And at the top: everything detaches; the echo hides.
	ui.pane.ScrollTo(0)
	ui.reconcilePromptPin()
	if ui.pinnedPrompt != "" || ui.promptEcho.GetText(true) != "" {
		t.Fatalf("echo did not hide above every prompt: pin=%q echo=%q",
			ui.pinnedPrompt, ui.promptEcho.GetText(true))
	}
}

// TestPromptAnchors_AppendMatchesFull pins anchor-table integrity across
// the incremental paths with no full rebuild sneaking into the captured
// state: after the baseline render, a stretch of pure-append refreshes
// (tool lines plus a mixed [tool, prompt] batch — the only way
// appendRows delta-adds a prompt anchor) must produce the same table
// an explicit full recompute of that state produces.
func TestPromptAnchors_AppendMatchesFull(t *testing.T) {
	ui := New("test")
	ui.AddPrompt("a reasonably long question that might wrap on narrower panes")
	ui.refreshMessages()
	if ui.tr.FullRenders() != 1 {
		t.Fatalf("baseline should be exactly one full render, got %d", ui.tr.FullRenders())
	}

	// Mixed batch with no refresh in between: the first new item is a
	// tool block (append path) and the batch also carries a prompt, so
	// appendRows must delta-add its anchor.
	ui.AddToolStart("1", "bash", "ls")
	ui.AddPrompt("second question in one mixed batch")
	ui.refreshMessages()
	if ui.tr.FullRenders() != 1 {
		t.Fatalf("mixed batch took the full-render path (%d rebuilds)", ui.tr.FullRenders())
	}
	if len(ui.tr.Anchors()) != 2 {
		t.Fatalf("expected a delta-added second anchor, got %+v", ui.tr.Anchors())
	}

	// Pure-delta tables, captured before any full-repaint trigger.
	appendedAnchors := append([]transcript.Anchor(nil), ui.tr.Anchors()...)
	appendedRows := append([]string(nil), ui.tr.Rows()...)

	// Force the full recompute in this exact state and diff both tables.
	ui.tr.RequestFull()
	ui.refreshMessages()
	fullAnchors := ui.tr.Anchors()
	fullRows := ui.tr.Rows()

	if len(appendedAnchors) != len(fullAnchors) {
		t.Fatalf("anchor counts differ: %d vs %d", len(appendedAnchors), len(fullAnchors))
	}
	for i := range fullAnchors {
		if appendedAnchors[i] != fullAnchors[i] {
			t.Fatalf("anchor %d drifted between delta and full: %+v vs %+v",
				i, appendedAnchors[i], fullAnchors[i])
		}
	}
	if len(appendedRows) != len(fullRows) {
		t.Fatalf("row counts differ: %d vs %d", len(appendedRows), len(fullRows))
	}
	for i := range fullRows {
		if appendedRows[i] != fullRows[i] {
			t.Fatalf("row %d drifted between delta and full: %q vs %q",
				i, appendedRows[i], fullRows[i])
		}
	}
}

// TestReconcilePromptPin_DeadlockSafety pins the freeze fix: tview
// invokes the AfterDraw hook while still holding the application lock,
// so the pin-change path must not take that lock again (SetFocus used
// to, freezing the UI at the first pin swap). A pin change is queued
// via the scroll position before the draw; the draw must return and
// leave focus untouched.
func TestReconcilePromptPin_DeadlockSafety(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(120, 30)
	ui := New("test")
	ui.App.SetScreen(sim)

	ui.AddPrompt("question one")
	ui.refreshMessages()
	if len(ui.tr.Anchors()) != 1 {
		t.Fatalf("expected one anchor, got %+v", ui.tr.Anchors())
	}

	// Content taller than the pane: the refresh's auto-scroll carries the
	// viewport past the prompt's seam, so the draw below has a pin change
	// pending (empty -> question one).
	ui.tokenMu.Lock()
	ui.pendingTokens.WriteString(strings.Repeat("word ", 800))
	ui.isStreaming.Store(true)
	ui.tokenMu.Unlock()
	if !ui.flushPendingTokens() {
		t.Fatalf("flush did not render the streamed response")
	}
	ui.refreshMessages()

	focusBefore := ui.App.GetFocus()

	watchdog := time.AfterFunc(10*time.Second, func() {
		panic("reconcilePromptPin deadlocked inside the application draw (app lock re-entered)")
	})
	defer watchdog.Stop()

	ui.App.ForceDraw()

	if ui.pinnedPrompt != "question one" {
		t.Fatalf("pin change did not land during the draw: %q", ui.pinnedPrompt)
	}
	if ui.App.GetFocus() != focusBefore {
		t.Fatalf("pin reconciliation stole focus")
	}

	// Painted-row validation: a second question past the wrapped response
	// gives an anchor whose Row comes from the prefix table; it must match
	// the row the pane actually painted (painted = innerTop + Row - offset).
	ui.AddPrompt("question two")
	ui.refreshMessages()
	ui.App.ForceDraw()

	ro := ui.pane.Offset()
	_, innerTop, _, innerH := ui.pane.GetInnerRect()
	// The second prompt's content row, located in the painted table —
	// the prefix table's anchor points at the item's first row (its
	// separator), so the content itself sits within the block.
	content := -1
	for i, ln := range ui.tr.Rows() {
		if strings.Contains(ln, "question two") {
			content = i
			break
		}
	}
	if content < 0 {
		t.Fatalf("second prompt not painted")
	}
	anchor := ui.tr.Anchors()[1]
	if content < anchor.Row || content > anchor.Row+2 {
		t.Fatalf("content row %d not inside the anchor block at %d", content, anchor.Row)
	}
	expected := innerTop + content - ro
	if expected < innerTop || expected >= innerTop+innerH {
		t.Fatalf("anchored row outside viewport: expected=%d innerTop=%d innerH=%d offset=%d",
			expected, innerTop, innerH, ro)
	}

	cells, w, _ := sim.GetContents()
	var b strings.Builder
	for x := 0; x < w; x++ {
		i := expected*w + x
		if i >= len(cells) || len(cells[i].Runes) == 0 {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(cells[i].Runes[0])
	}
	if !strings.Contains(strings.TrimRight(b.String(), " "), "question two") {
		t.Fatalf("anchor Row %d does not match the painted row %d (%q)",
			ui.tr.Anchors()[1].Row, expected, b.String())
	}
}
