package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

func TestRestoreHistoryRendersVisibleRowsInOrder(t *testing.T) {
	ui := New("test")
	ui.SetHistory([]HistoryItem{
		{Kind: HistoryUser, Text: "fix the flaky test"},
		{
			Kind:      HistoryAssistant,
			Text:      "on it",
			Reasoning: "first, reproduce",
			ToolCalls: []HistoryToolCall{
				{ID: "1", Name: "bash", Args: `{"command":"go test"}`, Summary: "1 test passed", Result: "ok"},
				{ID: "2", Name: "edit", Args: `{"path":"x.go"}`, Error: "error: denied"},
				{ID: "3", Name: "read", Args: `{"path":"y.go"}`, Interrupted: true},
			},
		},
		{Kind: HistoryUser, Text: "thanks"},
	})

	ui.restoreHistory()

	if ui.tr.Len() != 7 {
		t.Fatalf("transcript has %d items, want 7", ui.tr.Len())
	}
	if !ui.tr.ItemAt(0).IsPrompt || ui.tr.ItemAt(0).Prompt != "fix the flaky test" {
		t.Errorf("item 0 = %+v, want the historical user prompt with sticky-pin metadata", ui.tr.ItemAt(0))
	}
	if ui.tr.ItemAt(1).ReasBlock == nil {
		t.Error("item 1 should be the reasoning block")
	}
	if !ui.tr.ItemAt(2).Markdown || ui.tr.ItemAt(2).Text != "on it" {
		t.Errorf("item 2 = %+v, want assistant markdown", ui.tr.ItemAt(2))
	}

	firstTool := ui.tr.ItemAt(3).ToolBlock
	if firstTool == nil || firstTool.S() != toolblock.Done {
		t.Errorf("item 3 = %+v, want the completed tool block", ui.tr.ItemAt(3))
	}
	// The provider called this tool "1", the same value a live event would
	// use. It must not collide with the next live ToolEndEvent{ID: 1}.
	if strings.HasPrefix(firstTool.ID(), "1") {
		t.Errorf("restored tool id %q can collide with a live numeric id", firstTool.ID())
	}
	secondTool := ui.tr.ItemAt(4).ToolBlock
	if secondTool == nil || secondTool.S() != toolblock.Error {
		t.Errorf("item 4 = %+v, want the failed tool block", ui.tr.ItemAt(4))
	}
	thirdTool := ui.tr.ItemAt(5).ToolBlock
	if thirdTool == nil || thirdTool.S() != toolblock.Interrupted {
		t.Errorf("item 5 = %+v, want the interrupted tool block", ui.tr.ItemAt(5))
	}
	if !ui.tr.ItemAt(6).IsPrompt || ui.tr.ItemAt(6).Prompt != "thanks" {
		t.Errorf("item 6 = %+v, want the last user prompt", ui.tr.ItemAt(6))
	}
	// RequestFull is model-internal; the observable is that the first
	// repaint takes the full-rebuild branch (no empty painted window to
	// append onto), covered by TestRestoreHistoryThenLiveAppend below.
}

// TestRestoreHistoryLabelsMatchLive pins the collapsed text a resumed user
// sees: live failed blocks are labeled "<tool> ❌" (proxy.go), not with a
// result-derived summary, a persisted duration renders exactly as a live one,
// a call with no result renders Interrupted, and restored blocks never claim
// 0ms.
func TestRestoreHistoryLabelsMatchLive(t *testing.T) {
	ui := New("test")
	ui.SetHistory([]HistoryItem{{
		Kind: HistoryAssistant,
		ToolCalls: []HistoryToolCall{
			{ID: "call-ok", Name: "bash", Summary: "1 test passed", Result: "ok", Duration: 1400 * time.Millisecond},
			{ID: "call-err", Name: "bash", Summary: "exit status 1", Error: "error: exit status 1"},
			{ID: "call-orphan", Name: "read", Interrupted: true},
		},
	}})
	ui.restoreHistory()
	ui.refreshMessages()
	text := ui.tr.PaintedText()
	for _, want := range []string{"1 test passed", "1.4s", "bash ❌", "interrupted"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered transcript missing %q: %q", want, text)
		}
	}
	if got := ui.tr.ItemAt(2).ToolBlock; got == nil || got.S() != toolblock.Interrupted {
		t.Errorf("item 2 = %+v, want the interrupted tool block", ui.tr.ItemAt(2))
	}
	if strings.Contains(text, "exit status 1 ❌") {
		t.Errorf("error block used the computed summary instead of the tool name: %q", text)
	}
	if strings.Contains(text, "0ms") {
		t.Errorf("restored block claimed a measured duration: %q", text)
	}

	// The error body only appears when the failed block is expanded.
	// The toggle must go through the model mutator: it bumps the item's
	// generation so the wrapped-row cache re-renders the expanded body.
	ui.ToggleBlockByIndex(1)
	ui.refreshMessages()
	expanded := ui.tr.PaintedText()
	if !strings.Contains(expanded, "error: exit status 1") || !strings.Contains(expanded, "Duration: —") {
		t.Errorf("expanded error block lost its body or duration: %q", expanded)
	}
}

// TestRestoreHistoryThenLiveAppend pins the resume experience: restored
// rows are visible on first render, and the next live response appends
// after them instead of replacing the transcript.
func TestRestoreHistoryThenLiveAppend(t *testing.T) {
	ui := New("test")
	ui.SetHistory([]HistoryItem{
		{Kind: HistoryUser, Text: "earlier question"},
		{Kind: HistoryAssistant, Text: "earlier answer"},
	})

	ui.restoreHistory()
	ui.refreshMessages()
	before := ui.tr.PaintedText()
	if !strings.Contains(before, "earlier question") || !strings.Contains(before, "earlier answer") {
		t.Fatalf("first render missing restored history: %q", before)
	}

	ui.addAssistantResponse("new live answer")
	ui.refreshMessages()
	after := ui.tr.PaintedText()
	if !strings.Contains(after, "earlier question") || !strings.Contains(after, "new live answer") {
		t.Fatalf("live append lost restored history: %q", after)
	}
	if strings.Index(after, "earlier answer") > strings.Index(after, "new live answer") {
		t.Errorf("new response rendered before restored history: %q", after)
	}
}

// TestBootstrapHistory covers the startup wiring itself. Deleting the
// Run() call, forgetting markDirty, or failing to consume the history
// would leave the direct restoreHistory tests green but the feature dead.
func TestBootstrapHistory(t *testing.T) {
	ui := New("test")
	// A captured queue stands in for the live UI event loop: markDirty
	// must enqueue a refresh without needing App.Run to drain it.
	ui.uiEventCh = make(chan uiEvent, 4)
	ui.SetHistory([]HistoryItem{{Kind: HistoryUser, Text: "resumed prompt"}})

	ui.bootstrapHistory()

	if len(ui.history) != 0 {
		t.Errorf("history was not consumed: %+v", ui.history)
	}
	if ui.tr.Len() != 1 || ui.tr.ItemAt(0).Prompt != "resumed prompt" {
		t.Fatalf("transcript = %+v, want the restored prompt", ui.tr.ItemAt(0))
	}
	if !ui.needsRefresh.Load() {
		t.Error("bootstrapHistory must schedule a redraw")
	}
	if len(ui.uiEventCh) != 1 {
		t.Errorf("queued %d UI events, want one refresh", len(ui.uiEventCh))
	}

	// Calling it again is a no-op; a second Run must not duplicate rows.
	ui.bootstrapHistory()
	if ui.tr.Len() != 1 {
		t.Errorf("second bootstrap duplicated history: %d items", ui.tr.Len())
	}
}

func TestRestoreHistoryEmpty(t *testing.T) {
	ui := New("test")
	ui.SetHistory(nil)
	ui.restoreHistory()
	if ui.tr.Len() != 0 {
		t.Errorf("transcript has %d items, want empty", ui.tr.Len())
	}
}
