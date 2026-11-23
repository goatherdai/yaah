package yaah

import (
	"testing"

	"github.com/goatherdai/yaah/internal/tui"
	"github.com/goatherdai/yaah/internal/types"
)

func TestTUIHistoryFromMessagesVisibleRows(t *testing.T) {
	history := []types.Message{
		types.SystemMsg("system: assembler prompt"), // never streamed live
		types.UserMsg("fix the flaky test"),
		{Role: "assistant", Content: "thinking hard", ReasoningContent: "private chain"},
		{
			Role:    "assistant",
			Content: "[tool:read] {\"path\":\"foo.go\"}",
			ToolCalls: []types.ToolCall{{
				ID:       "call-1",
				Function: types.ToolCallFn{Name: "read", Arguments: `{"path":"foo.go"}`},
			}},
		},
		types.ToolResultMsg("call-1", "read", "contents of foo.go"),
		types.UserMsg("looks good, ship it"),
	}

	items := tuiHistoryFromMessages(history)
	if len(items) != 4 {
		t.Fatalf("got %d history items, want 4: %+v", len(items), items)
	}

	if items[0].Kind != tui.HistoryUser || items[0].Text != "fix the flaky test" {
		t.Errorf("item 0 = %+v, want the operator prompt", items[0])
	}
	if items[1].Kind != tui.HistoryAssistant || items[1].Text != "thinking hard" || items[1].Reasoning != "private chain" {
		t.Errorf("item 1 = %+v, want assistant markdown plus reasoning", items[1])
	}

	item2 := items[2]
	if item2.Kind != tui.HistoryAssistant {
		t.Errorf("item 2 kind = %v, want assistant", item2.Kind)
	}
	if item2.Text != "" {
		t.Errorf("synthetic tool-carrier text leaked into the transcript: %q", item2.Text)
	}
	if len(item2.ToolCalls) != 1 {
		t.Fatalf("item 2 tool calls = %+v, want one", item2.ToolCalls)
	}
	call := item2.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "read" || call.Result != "contents of foo.go" {
		t.Errorf("tool call = %+v, want the call paired with its result", call)
	}
	if call.Summary == "" {
		t.Error("tool call summary should be computed for the collapsed block")
	}
	if call.Error != "" || call.ResultMissing {
		t.Errorf("successful call flagged as failed or missing: %+v", call)
	}

	if items[3].Kind != tui.HistoryUser || items[3].Text != "looks good, ship it" {
		t.Errorf("item 3 = %+v, want the last operator prompt", items[3])
	}
}

func TestTUIHistoryFromMessagesExcludedRows(t *testing.T) {
	// Each row here is a conversation internal that neither ACP replay nor
	// the live TUI streamed to the client. None may reach the transcript.
	cases := []struct {
		name    string
		message types.Message
	}{
		{"system", types.SystemMsg("compaction summary")},
		{"empty user", types.UserMsg("")},
		{"steer", types.UserMsg("[STEER] nudge the agent")},
		{"supervisor", types.UserMsg("SUPERVISOR: retry after failure")},
		{"conflict", types.UserMsg("CONFLICT: workers disagreed")},
		{"background", types.UserMsg("[BACKGROUND worker] job finished")},
		{"tool result", types.ToolResultMsg("call-1", "read", "orphaned result")},
		{"synthetic carrier only", types.Message{Role: "assistant", Content: "[tool:read] {}"}},
		{"empty assistant", types.Message{Role: "assistant", Content: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := tuiHistoryFromMessages([]types.Message{tc.message})
			if len(items) != 0 {
				t.Fatalf("internal row leaked into history: %+v", items)
			}
		})
	}
}

func TestTUIHistoryFromMessagesToolCallStates(t *testing.T) {
	assistantWith := func(id string) types.Message {
		return types.Message{
			Role: "assistant",
			ToolCalls: []types.ToolCall{{
				ID:       id,
				Function: types.ToolCallFn{Name: "bash", Arguments: `{"command":"echo hi"}`},
			}},
		}
	}

	t.Run("paired success", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{
			assistantWith("call-ok"),
			types.ToolResultMsg("call-ok", "bash", "all tests passed"),
		})
		call := requireOneCall(t, items)
		if call.Error != "" || call.ResultMissing || call.Summary == "" {
			t.Errorf("call = %+v, want a summarized success", call)
		}
	})

	t.Run("paired error", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{
			assistantWith("call-err"),
			types.ToolResultMsg("call-err", "bash", "error: exit status 1"),
		})
		call := requireOneCall(t, items)
		if call.Error != "error: exit status 1" {
			t.Errorf("error = %q, want the persisted error text", call.Error)
		}
		// The live TUI labels failed blocks with the tool name; no computed
		// result summary may leak in.
		if call.Summary != "" {
			t.Errorf("error call summary = %q, want empty", call.Summary)
		}
	})

	t.Run("uppercase output is not an error", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{
			assistantWith("call-out"),
			types.ToolResultMsg("call-out", "bash", "ERROR: disk almost full"),
		})
		call := requireOneCall(t, items)
		if call.Error != "" {
			t.Errorf("successful output misclassified as error: %q", call.Error)
		}
	})

	t.Run("result not retained", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{assistantWith("call-orphan")})
		call := requireOneCall(t, items)
		if !call.ResultMissing {
			t.Errorf("call = %+v, want ResultMissing", call)
		}
	})

	t.Run("empty-id call skipped", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{assistantWith("")})
		if len(items) != 0 {
			t.Errorf("ID-less call should be skipped like ACP: %+v", items)
		}
	})

	t.Run("text and calls coexist", func(t *testing.T) {
		items := tuiHistoryFromMessages([]types.Message{{
			Role:    "assistant",
			Content: "here is the file",
			ToolCalls: []types.ToolCall{{
				ID:       "call-read",
				Function: types.ToolCallFn{Name: "read", Arguments: "{}"},
			}},
		}})
		if len(items) != 1 || items[0].Text != "here is the file" {
			t.Fatalf("assistant text lost: %+v", items)
		}
		if len(items[0].ToolCalls) != 1 {
			t.Errorf("tool call lost: %+v", items[0])
		}
	})
}

func TestTUIHistoryFromMessagesFreshSession(t *testing.T) {
	if got := tuiHistoryFromMessages(nil); len(got) != 0 {
		t.Errorf("fresh session history = %+v, want empty", got)
	}
}

func requireOneCall(t *testing.T, items []tui.HistoryItem) tui.HistoryToolCall {
	t.Helper()
	if len(items) != 1 || len(items[0].ToolCalls) != 1 {
		t.Fatalf("got %+v, want one assistant item with one call", items)
	}
	return items[0].ToolCalls[0]
}
