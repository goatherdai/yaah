package yaah

import (
	"strings"

	agentcontext "github.com/goatherdai/yaah/internal/agent/context"
	"github.com/goatherdai/yaah/internal/toolfmt"
	"github.com/goatherdai/yaah/internal/tui"
	"github.com/goatherdai/yaah/internal/types"
)

// toolErrorPrefix is the exact lowercase prefix the agent loop writes for
// normalized tool failures (internal/agent/agent_dispatch.go).
//
// It is the LEGACY fallback only: it decides a row whose persisted status is
// unknown, i.e. one written before tool outcomes were persisted (yaah-bmv3).
// Such a row still carries no error flag, so the prefix is the best evidence
// available and a successful command whose output starts with exactly
// "error: " stays misclassified there — exactly the pre-outcome behaviour
// pre-existing sessions must keep. A row with a persisted status is never
// classified by its text (see applyToolResult).
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
				res, ok := results[call.ID]
				applyToolResult(&hc, res, ok)
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

// applyToolResult settles one restored call from its persisted result row.
//
// The persisted status is authoritative: a row carrying one is rendered from
// it, never from the result text — a successful command may print
// "error: ...", and a failure may print anything at all. A row whose status
// is unknown predates the outcome column, so the documented legacy prefix
// rule decides and that session renders exactly as it always did.
//
// A call with NO result row is interrupted: there is no status to read and,
// structurally, the turn ended before a result existed for that call. It
// must not be replayed as an implied success, so it carries no duration and
// no summary.
func applyToolResult(hc *tui.HistoryToolCall, res types.Message, found bool) {
	if !found {
		hc.Interrupted = true
		return
	}
	hc.Result = res.Content
	hc.Duration = res.Duration
	switch res.Outcome {
	case types.ToolOutcomeOK:
		hc.Summary = toolfmt.Summary(hc.Name, hc.Args, res.Content)
	case types.ToolOutcomeError:
		hc.Error = res.Content
	case types.ToolOutcomeInterrupted, types.ToolOutcomeCancelled:
		// Neither is written by the engine today (see types.ToolOutcome):
		// a cancelled call lands here only once the never-started slice
		// publishes its events. Both mean "did not run to a result", and the
		// live view has no separate cancelled glyph, so they project onto the
		// Interrupted state rather than inventing one.
		hc.Interrupted = true
	case types.ToolOutcomeUnknown:
		// Legacy row: no status was persisted, so the prefix rule decides
		// exactly as it did before the outcome column existed.
		if strings.HasPrefix(res.Content, toolErrorPrefix) {
			hc.Error = res.Content
		} else {
			hc.Summary = toolfmt.Summary(hc.Name, hc.Args, res.Content)
		}
	}
}
