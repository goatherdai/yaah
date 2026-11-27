package tui

import (
	"strconv"

	"github.com/goatherdai/yaah/internal/tui/components/reasoning"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// HistoryKind identifies the visible role of a restored conversation row.
// System rows and other conversation internals are filtered out by the
// driver before they reach the TUI — the same rows ACP's session/load
// replay skips.
type HistoryKind uint8

const (
	HistoryUser HistoryKind = iota
	HistoryAssistant
)

// HistoryToolCall is one tool call from a restored assistant row, paired
// with its result (when the conversation contains one).
type HistoryToolCall struct {
	ID            string
	Name          string
	Args          string
	Summary       string
	Result        string
	Error         string
	ResultMissing bool // no result row survived compaction
}

// HistoryItem is one visible row of a conversation being restored into the
// transcript. The driver builds these from the session's persisted messages;
// the TUI stays independent of the storage and message wire types.
type HistoryItem struct {
	Kind      HistoryKind
	Text      string // user text, or assistant markdown
	Reasoning string // assistant reasoning, rendered as a collapsible block
	ToolCalls []HistoryToolCall
}

// SetHistory installs the conversation to seed into the transcript. It must
// be called before Run; restoreHistory consumes it on the main goroutine
// before the event loops start delivering live events.
func (t *App) SetHistory(items []HistoryItem) {
	t.history = items
}

// restoreHistory appends the pre-loaded conversation to conversationLog.
// It runs on the setup goroutine (before App.Run), so direct mutation is
// safe; callers schedule the resulting redraw with markDirty once the UI
// event loops are running.
func (t *App) restoreHistory() {
	for i, item := range t.history {
		switch item.Kind {
		case HistoryUser:
			t.appendPromptItem(item.Text)
		case HistoryAssistant:
			if item.Reasoning != "" {
				id := "history-reason-" + strconv.Itoa(i)
				t.conversationLog = append(t.conversationLog, convItem{
					reasoningBlock: reasoning.New(id, item.Reasoning, 0, t.Theme),
				})
			}
			if item.Text != "" {
				t.conversationLog = append(t.conversationLog, convItem{
					text:       item.Text,
					isMarkdown: true,
				})
			}
			for j, call := range item.ToolCalls {
				// Always namespace restored IDs. Live tool events use bare
				// per-run counters ("1", "2", ...); a provider call id can
				// look identical, and AddToolEnd scans from the front of the
				// log, so an unprefixed restored id would capture a live
				// tool's result and leave the live block stuck as Running.
				id := "history-tool-" + strconv.Itoa(i) + "-" + strconv.Itoa(j)
				tb := toolblock.New(id, call.Name, call.Args, t.Theme)
				if call.Error != "" {
					// Match the live path (proxy.go): the failed block carries
					// the tool name, not the computed result summary.
					tb.Fail(call.Name+" \u274C", call.Error)
				} else {
					summary := call.Summary
					if summary == "" {
						summary = call.Name
					}
					if call.ResultMissing {
						summary += " (result not retained)"
					}
					tb.Complete(summary, call.Result)
				}
				// A restored row has no timing data; do not claim 0ms.
				tb.SetDurationLabel("\u2014")
				t.conversationLog = append(t.conversationLog, convItem{toolBlock: tb})
			}
		}
	}
	// The log was seeded in bulk; the incremental append fast path must not
	// try to diff it against an empty rendered buffer.
	t.needsFullRender.Store(true)
}

// bootstrapHistory seeds the transcript and schedules its first redraw.
// Run calls it after the UI event loops exist but before App.Run drains
// them, so restored rows are already in conversationLog when the first
// live event callback runs. markDirty sees a live queue and enqueues one
// refresh instead of falling back to a goroutine blocked on tview.
func (t *App) bootstrapHistory() {
	if len(t.history) == 0 {
		return
	}
	t.restoreHistory()
	t.history = nil
	t.markDirty()
}
