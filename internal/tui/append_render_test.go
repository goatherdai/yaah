package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The append/render contract these tests pin: refreshMessages repaints
// through the transcript model, and tail-only batches take the append
// fast path — wrapped rows are added, nothing existing is re-wrapped
// (proven by FullRenders not moving). In-place block mutations force
// the full rebuild.

// TestRefreshMessages_AppendsNewTailOnly verifies tail-only rendering:
// appending items paints just the new content without disturbing what is
// already painted. Text items append too, so the fast path is proven by
// FullRenders not moving.
func TestRefreshMessages_AppendsNewTailOnly(t *testing.T) {
	ui := New("test")

	ui.appendMessage("hello there")
	ui.refreshMessages()
	if n := ui.tr.Len(); n != 1 {
		t.Fatalf("expected 1 transcript item after first render, got %d", n)
	}
	before := ui.tr.PaintedText()
	if !strings.Contains(before, "hello there") {
		t.Fatalf("first render missing user message; got %q", before)
	}
	fullRenders := ui.tr.FullRenders()

	ui.addAssistantResponse("streamed **response** tail")
	ui.refreshMessages()
	if got := ui.tr.FullRenders(); got != fullRenders {
		t.Fatalf("text item took the full-render path (%d rebuilds, want %d)", got, fullRenders)
	}
	after := ui.tr.PaintedText()
	if !strings.HasPrefix(after, before) {
		t.Fatalf("append path rewrote existing rows:\nbefore=%q\nafter=%q", before, after)
	}
	if !strings.Contains(after, "streamed") || !strings.Contains(after, "response") {
		t.Fatalf("append path dropped new content; got %q", after)
	}
}

// msgColumn trims a painted row to the messages column, cutting at the
// scrollbar's track/thumb glyphs and the right pane's border glyph when
// present — all pane chrome, never content.
func msgColumn(row string) string {
	if i := strings.IndexAny(row, "│┃"); i >= 0 {
		row = row[:i]
	}
	return row
}

// paintScreen draws the root onto sim and returns the visible rows,
// right-trimmed. The shared probe for every painted-layout assertion:
// only a simulated-screen draw proves what the user actually sees.
func paintScreen(t *testing.T, ui *App, sim tcell.SimulationScreen, height int) []string {
	t.Helper()
	ui.Root.Draw(sim)
	sim.Show()
	cells, w, _ := sim.GetContents()
	rows := make([]string, 0, height)
	for y := 0; y < height; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			i := y*w + x
			if i >= len(cells) || len(cells[i].Runes) == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(cells[i].Runes[0])
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return rows
}

// newPaintedUI builds an App with a simulated screen sized 120x40, lays
// the root out once, and returns the app with a paint helper bound to
// that screen. Tests that assert painted layout must draw through the
// pane, not the model's row table: the pane owns the viewport.
func newPaintedUI(t *testing.T) (*App, func() []string) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(120, 40)
	ui := New("test")
	ui.Root.SetRect(0, 0, 120, 40)
	ui.Root.Draw(sim)
	return ui, func() []string { return paintScreen(t, ui, sim, 40) }
}

// TestRefreshMessages_ResponseChunkHasSeparator verifies the painted
// output of a flushed assistant response on the APPEND path: a blank
// line must separate it from the preceding tool line, and its first row
// must carry no stray leading space. The TextView path needed the
// appendChunk dance to keep uniseg's resumed parse from collapsing the
// separator into a one-space indent; the pane wraps each item
// independently, so the assertions pin that the new construction holds
// the same painted shape.
func TestRefreshMessages_ResponseChunkHasSeparator(t *testing.T) {
	ui, paint := newPaintedUI(t)

	ui.appendMessage("comment on the bead please")
	ui.AddToolStart("1", "bash", "bd comment yaah-95ri.7")
	ui.AddToolEnd("1", "bash · bd comment", "ok", 0)
	ui.refreshMessages()
	fullRenders := ui.tr.FullRenders()

	ui.tokenMu.Lock()
	ui.pendingTokens.WriteString("Posted to the bead. Here's the complete picture — what the fix looks like and your stopgap.")
	ui.isStreaming.Store(true)
	ui.tokenMu.Unlock()
	if !ui.flushPendingTokens() {
		t.Fatalf("flush did not render the streamed response")
	}
	ui.refreshMessages()
	if got := ui.tr.FullRenders(); got != fullRenders {
		t.Fatalf("flushed text took the full-render path (%d rebuilds, want %d)", got, fullRenders)
	}
	rows := paint()

	posted := -1
	for i, r := range rows {
		if strings.Contains(r, "Posted to the bead") {
			posted = i
			break
		}
	}
	if posted < 2 {
		t.Fatalf("response row not found in painted output: %v", rows)
	}
	if strings.HasPrefix(msgColumn(rows[posted]), " ") {
		t.Errorf("response first row is visually indented by a stray space: %q", rows[posted])
	}
	if strings.TrimSpace(msgColumn(rows[posted-1])) != "" {
		t.Errorf("no blank line between response and preceding tool line: %q / %q", rows[posted-2], rows[posted-1])
	}
	if !strings.Contains(msgColumn(rows[posted-2]), "✓") {
		t.Errorf("expected tool line above the response separator: %q", rows[posted-2])
	}

	// The layout must be stable: a subsequent full render (forced by the
	// next tool completing) may not shift or re-indent the response.
	ui.AddToolStart("2", "bash", "git status")
	ui.AddToolEnd("2", "bash · git status", "clean", 0)
	ui.refreshMessages()
	rows2 := paint()
	posted2 := -1
	for i, r := range rows2 {
		if strings.Contains(r, "Posted to the bead") {
			posted2 = i
			break
		}
	}
	if posted2 != posted {
		t.Errorf("response shifted between renders: %d -> %d", posted, posted2)
	}
	if posted2 >= 0 && strings.HasPrefix(msgColumn(rows2[posted2]), " ") {
		t.Errorf("full render indented the response: %q", rows2[posted2])
	}
}

// TestRefreshMessages_ToolChunkAppends verifies the tool-line fast path:
// a batch whose first new item is a tool block is appended onto the
// existing row table — the append branch is proven by FullRenders not
// moving.
func TestRefreshMessages_ToolChunkAppends(t *testing.T) {
	ui := New("test")

	ui.appendMessage("run something")
	ui.AddToolStart("1", "bash", "ls")
	ui.refreshMessages()
	before := ui.tr.PaintedText()
	fullRenders := ui.tr.FullRenders()

	ui.AddToolStart("2", "grep", "pattern")
	ui.refreshMessages()

	if got := ui.tr.FullRenders(); got != fullRenders {
		t.Fatalf("tool chunk took the full-render path (%d rebuilds, want %d)", got, fullRenders)
	}
	after := ui.tr.PaintedText()
	if !strings.HasPrefix(after, before) {
		t.Fatalf("append path rewrote existing rows:\nbefore=%q\nafter=%q", before, after)
	}
	if !strings.Contains(after, "grep") {
		t.Fatalf("append path dropped new tool chunk; got %q", after)
	}
}

// TestRefreshMessages_MixedBatchAppendsToolChunk verifies a batch that
// starts with a tool block but contains a later text item (e.g. a tool
// start coalesced with a flush). The painted-rows assertions are the real
// check: the row table cannot see paint-time behavior, so only a
// simulated-screen draw proves the text item's separator survived.
func TestRefreshMessages_MixedBatchAppendsToolChunk(t *testing.T) {
	ui, paint := newPaintedUI(t)

	ui.appendMessage("run something")
	ui.refreshMessages()
	fullRenders := ui.tr.FullRenders()

	// Tool added first, then a streamed response flushed, with no
	// refresh in between — one mixed tail batch.
	ui.AddToolStart("1", "bash", "ls")
	ui.tokenMu.Lock()
	ui.pendingTokens.WriteString("the streamed answer")
	ui.isStreaming.Store(true)
	ui.tokenMu.Unlock()
	if !ui.flushPendingTokens() {
		t.Fatalf("flush did not consume pending tokens")
	}
	ui.refreshMessages()
	if got := ui.tr.FullRenders(); got != fullRenders {
		t.Fatalf("mixed batch took the full-render path (%d rebuilds, want %d)", got, fullRenders)
	}

	// Paint through the pane and assert the text item's blank-line
	// separator survived: no leading space on its first row, blank row
	// between it and the tool line above.
	rows := paint()
	answer := -1
	for i, r := range rows {
		if strings.Contains(r, "the streamed answer") {
			answer = i
			break
		}
	}
	if answer < 2 {
		t.Fatalf("streamed answer row not painted: %v", rows)
	}
	if strings.HasPrefix(msgColumn(rows[answer]), " ") {
		t.Errorf("mixed batch collapsed the separator: %q", rows[answer])
	}
	if strings.TrimSpace(msgColumn(rows[answer-1])) != "" {
		t.Errorf("no blank row between tool line and appended text: %q / %q", rows[answer-2], rows[answer-1])
	}
	if !strings.Contains(msgColumn(rows[answer-2]), "💻") {
		t.Errorf("expected tool line above the separator: %q", rows[answer-2])
	}

	// Row-table level: append preserved prior content.
	if after := ui.tr.PaintedText(); !strings.Contains(after, "run something") {
		t.Fatalf("append path dropped existing content: %q", after)
	}
}

// TestAddToolEnd_ForcesFullRender verifies that mutating an existing
// block in place invalidates the append fast path.
func TestAddToolEnd_ForcesFullRender(t *testing.T) {
	ui := New("test")

	ui.appendMessage("run something")
	ui.refreshMessages()

	ui.AddToolStart("42", "bash", "ls")
	ui.refreshMessages()
	fullRenders := ui.tr.FullRenders()

	ui.AddToolEnd("42", "bash", "file output", 0)
	ui.refreshMessages()
	if got := ui.tr.FullRenders(); got != fullRenders+1 {
		t.Fatalf("completing an EXISTING tool block must force a full rebuild (full renders %d, want %d)", got, fullRenders+1)
	}
}

// TestRepaint_ResizesRewrapRows pins the resize seam: the pane paints
// pre-wrapped rows, so a width change must re-wrap them before the frame
// that would show stale-width content. BeforeDraw performs the repaint;
// after it, every painted row fits the new width.
func TestRepaint_ResizesRewrapRows(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(120, 40)

	ui := New("test")
	ui.Root.SetRect(0, 0, 120, 40)
	ui.Root.Draw(sim)

	ui.appendMessage(strings.Repeat("word ", 60)) // 300 chars: wraps at 120, more rows at 60
	ui.refreshMessages()
	rowsAt120 := len(ui.tr.Rows())
	if rowsAt120 == 0 {
		t.Fatalf("nothing painted")
	}

	// Halve the pane's width and lay out again, the way a resize does.
	// The AfterDraw hook's body runs after the layout has cascaded.
	sim.SetSize(60, 40)
	ui.Root.SetRect(0, 0, 60, 40)
	ui.Root.Draw(sim)
	ui.repaintOnResize(sim)
	if w := ui.tr.PaintedWidth(); w > 60 {
		t.Fatalf("rows still wrapped at stale width %d after resize", w)
	}
	if got := len(ui.tr.Rows()); got <= rowsAt120 {
		t.Fatalf("narrower pane must wrap into more rows: %d at width 60, %d at width 120", got, rowsAt120)
	}
}

// TestEnqueueUIEventDirect_AbandonsAtShutdown verifies that in-flight
// fallback work is abandoned once the background loops are stopped, so no
// goroutine blocks forever on QueueUpdate of a stopped application.
func TestEnqueueUIEventDirect_AbandonsAtShutdown(t *testing.T) {
	ui := New("test")
	done := make(chan struct{})
	ui.bgDone = done
	close(done)

	ran := false
	ui.enqueueUIEventDirect(false, func() { ran = true })

	time.Sleep(50 * time.Millisecond)
	if ran {
		t.Fatalf("fallback work executed after shutdown")
	}
}

// TestSetCurrentPrompt_EchoWrapsAndTruncates pins the prompt echo
// behavior: short prompts fit on one row, long prompts truncate with
// an ellipsis instead of growing past promptEchoMaxLines.
func TestSetCurrentPrompt_EchoWrapsAndTruncates(t *testing.T) {
	ui := New("test")

	ui.SetCurrentPrompt("short prompt")
	short := ui.promptEcho.GetText(true)
	if !strings.Contains(short, "short prompt") {
		t.Fatalf("echo missing text: %q", short)
	}
	if strings.Contains(short, "…") {
		t.Errorf("short prompt should not truncate: %q", short)
	}

	ui.SetCurrentPrompt(strings.Repeat("x", 500))
	long := ui.promptEcho.GetText(true)
	if !strings.HasSuffix(long, "…") {
		t.Errorf("long prompt should end with ellipsis: %q", long)
	}
	if got := len([]rune(strings.TrimSuffix(long, "…"))); got > 500 {
		t.Errorf("truncated echo unexpectedly long: %d runes", got)
	}
}
