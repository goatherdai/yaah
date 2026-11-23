package yaah

import (
	"strings"

	agentcontext "github.com/goatherdai/yaah/internal/agent/context"
	"github.com/goatherdai/yaah/internal/toolfmt"
	"github.com/goatherdai/yaah/internal/tui"
	"github.com/goatherdai/yaah/internal/types"
)

// toolErrorPrefix is the exact lowercase prefix the agent loop writes for
// normalized tool failures (internal/agent/agent_dispatch.go). Persisted
// result rows carry no error flag, so this is a heuristic: a successful
// command whose output starts with exactly "error: " is still misclassified.
// Matching case-sensitively only avoids the broader false-positive class
// ("ERROR:", leading whitespace). yaah-bmv3 tracks persisting an explicit
// flag so replay can stop guessing.
const toolErrorPrefix = "error: "

// tuiHistoryFromMessages projects a persisted conversation onto the
// TUI's history items. It mirrors the rows ACP's session/load replay
// surfaces, plus reasoning and tool results (which ACP does not replay).
// It skips what a live run never streamed: system rows, synthetic
// tool-call carriers, agent-internal user rows, and bare tool-result
// rows — those are folded into the matching call instead.
func tuiHistoryFromMessages(history []types.Message) []tui.HistoryItem {
	results := make(map[string]types.Message, len(history))
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m
		}
	}

	items := make([]tui.HistoryItem, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case "user":
			if m.Content == "" || agentcontext.IsInternalUserContent(m.Content) {
				continue
			}
			items = append(items, tui.HistoryItem{Kind: tui.HistoryUser, Text: m.Content})
		case "assistant":
			text := m.Content
			if len(m.ToolCalls) > 0 && agentcontext.IsSyntheticToolCarrier(text) {
				text = ""
			}

			item := tui.HistoryItem{
				Kind:      tui.HistoryAssistant,
				Text:      text,
				Reasoning: m.ReasoningContent,
			}
			for _, call := range m.ToolCalls {
				// ACP skips ID-less calls: they cannot be paired with a
				// result, so replaying them would show a block with no
				// outcome. Keep the same rule here.
				if call.ID == "" {
					continue
				}
				// The live TUI routes delegate calls to the Subagents pane
				// and never adds a conversation block for them (proxy.go).
				if call.Function.Name == "delegate" {
					continue
				}
				hc := tui.HistoryToolCall{
					ID:   call.ID,
					Name: call.Function.Name,
					Args: call.Function.Arguments,
				}
				if res, ok := results[call.ID]; ok {
					hc.Result = res.Content
					if strings.HasPrefix(res.Content, toolErrorPrefix) {
						hc.Error = res.Content
					} else {
						hc.Summary = toolfmt.Summary(call.Function.Name, call.Function.Arguments, res.Content)
					}
				} else {
					// Compaction may drop a result while keeping the call.
					// Say so rather than implying a successful empty result.
					hc.ResultMissing = true
				}
				item.ToolCalls = append(item.ToolCalls, hc)
			}

			if item.Text == "" && item.Reasoning == "" && len(item.ToolCalls) == 0 {
				continue
			}
			items = append(items, item)
		}
	}
	return items
}
