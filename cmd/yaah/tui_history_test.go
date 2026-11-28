package yaah

import (
	"testing"
	"time"

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
	if call.Error != "" || call.Interrupted {
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
		{"delegate call", types.Message{
			Role:      "assistant",
			ToolCalls: []types.ToolCall{{ID: "call-delegate", Function: types.ToolCallFn{Name: "delegate", Arguments: "{}"}}},
		}},
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

// A prefix alone is not enough to identify a synthetic carrier: the
// persister only writes that placeholder when tool calls are present.
// Assistant text that happens to start with "[tool:" is a real reply.
func TestTUIHistoryFromMessagesCarrierPrefixWithoutCallsIsVisible(t *testing.T) {
	items := tuiHistoryFromMessages([]types.Message{{
		Role:    "assistant",
		Content: "[tool:question] asked",
	}})
	if len(items) != 1 || items[0].Text != "[tool:question] asked" {
		t.Fatalf("real assistant text was filtered as a carrier: %+v", items)
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
		if call.Error != "" || call.Interrupted || call.Summary == "" {
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

	t.Run("no result row is interrupted", func(t *testing.T) {
		// A call with no matching result row has no outcome to read and,
		// structurally, never got a result: it must not replay as an
		// implied success (yaah-77f1).
		items := tuiHistoryFromMessages([]types.Message{assistantWith("call-orphan")})
		call := requireOneCall(t, items)
		if !call.Interrupted {
			t.Errorf("call = %+v, want Interrupted", call)
		}
		if call.Duration != 0 || call.Summary != "" || call.Error != "" {
			t.Errorf("interrupted call carries outcome data it cannot have: %+v", call)
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

// TestTUIHistoryFromMessagesOutcomeStatus pins the resume projection onto the
// PERSISTED status (yaah-77f1, folding in yaah-bmv3): a row carrying one is
// rendered from it in both directions, and the "error: " prefix survives only
// as the fallback for rows whose status is unknown.
func TestTUIHistoryFromMessagesOutcomeStatus(t *testing.T) {
	assistantWith := func(id string) types.Message {
		return types.Message{
			Role: "assistant",
			ToolCalls: []types.ToolCall{{
				ID:       id,
				Function: types.ToolCallFn{Name: "bash", Arguments: `{"command":"run"}`},
			}},
		}
	}
	resultWith := func(id, content string, outcome types.ToolOutcome, d time.Duration) types.Message {
		m := types.ToolResultMsg(id, "bash", content)
		m.Outcome = outcome
		m.Duration = d
		return m
	}

	tests := []struct {
		name string
		// the row the conversation holds for the call (nil = none)
		result *types.Message
		wantOK bool // rendered as a summarized success
		// wantError/wantInterrupted are the other two distinct outcomes
		wantError       bool
		wantInterrupted bool
		wantDuration    time.Duration
	}{
		{
			// The former misclassification, direction 1: content that LOOKS
			// like a failure but the engine recorded ok.
			name:         "ok status beats error-looking content",
			result:       ptrTo(resultWith("call-1", "error: this is the program's own output", types.ToolOutcomeOK, 2*time.Second)),
			wantOK:       true,
			wantDuration: 2 * time.Second,
		},
		{
			// Direction 2: a failure whose text carries no prefix at all.
			name:         "error status beats prefix-less content",
			result:       ptrTo(resultWith("call-1", "tool blew up unexpectedly", types.ToolOutcomeError, 350*time.Millisecond)),
			wantError:    true,
			wantDuration: 350 * time.Millisecond,
		},
		{
			// Pre-outcome rows keep today's behaviour exactly.
			name:         "legacy prefix row still classifies as error",
			result:       ptrTo(resultWith("call-1", "error: exit status 1", types.ToolOutcomeUnknown, 0)),
			wantError:    true,
			wantDuration: 0,
		},
		{
			// ... including the misclassification it always had: that is
			// what "renders exactly as before" means.
			name:         "legacy prefix-less row still classifies as ok",
			result:       ptrTo(resultWith("call-1", "all good", types.ToolOutcomeUnknown, 0)),
			wantOK:       true,
			wantDuration: 0,
		},
		{
			name:            "no result row is interrupted",
			result:          nil,
			wantInterrupted: true,
		},
		{
			// Rows the engine does not write today (child 7 will): a result
			// row that carries interrupted/cancelled projects onto the
			// Interrupted render, never onto an implied success.
			name:            "interrupted row renders interrupted",
			result:          ptrTo(resultWith("call-1", "error: context canceled", types.ToolOutcomeInterrupted, 0)),
			wantInterrupted: true,
		},
		{
			name:            "cancelled row renders interrupted",
			result:          ptrTo(resultWith("call-1", "cancelled", types.ToolOutcomeCancelled, 0)),
			wantInterrupted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			history := []types.Message{assistantWith("call-1")}
			if tc.result != nil {
				history = append(history, *tc.result)
			}
			call := requireOneCall(t, tuiHistoryFromMessages(history))

			if got := call.Error != ""; got != tc.wantError {
				t.Errorf("Error = %q, want error = %v", call.Error, tc.wantError)
			}
			if call.Interrupted != tc.wantInterrupted {
				t.Errorf("Interrupted = %v, want %v", call.Interrupted, tc.wantInterrupted)
			}
			if got := call.Summary != ""; got != tc.wantOK {
				t.Errorf("Summary = %q, want summarized = %v", call.Summary, tc.wantOK)
			}
			if call.Duration != tc.wantDuration {
				t.Errorf("Duration = %v, want %v", call.Duration, tc.wantDuration)
			}
			// Exactly one outcome is set: the states are distinct renders.
			set := 0
			for _, on := range []bool{call.Error != "", call.Interrupted, call.Summary != ""} {
				if on {
					set++
				}
			}
			if set != 1 {
				t.Errorf("call = %+v, want exactly one of ok/error/interrupted", call)
			}
		})
	}
}

func ptrTo(m types.Message) *types.Message { return &m }
