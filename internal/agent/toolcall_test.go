package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// toolcall_test.go — the identity a tool call carries on its start and end
// events (yaah-77f1). A fresh Loop is built per prompt, so the dispatch
// ordinal restarts at 1 on every run; the key must not.

func TestToolCallKey(t *testing.T) {
	tests := []struct {
		name           string
		scopeA, scopeB string
		ordinalA       int64
		ordinalB       int64
		wantEqual      bool
	}{
		{
			name:   "same interaction reuses one key for one ordinal",
			scopeA: "run-a", scopeB: "run-a",
			ordinalA: 1, ordinalB: 1,
			wantEqual: true,
		},
		{
			// The defect: a later prompt's first call reuses ordinal 1.
			name:   "the same ordinal in another interaction cannot collide",
			scopeA: "run-a", scopeB: "run-b",
			ordinalA: 1, ordinalB: 1,
			wantEqual: false,
		},
		{
			name:   "different ordinals within one interaction cannot collide",
			scopeA: "run-a", scopeB: "run-a",
			ordinalA: 1, ordinalB: 2,
			wantEqual: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := toolCallKey(withToolCallScope(context.Background(), tc.scopeA), tc.ordinalA)
			b := toolCallKey(withToolCallScope(context.Background(), tc.scopeB), tc.ordinalB)
			if got := a == b; got != tc.wantEqual {
				t.Errorf("keys %q and %q equal = %v, want %v", a, b, got, tc.wantEqual)
			}
		})
	}
}

// toolCallRunEvents runs one prompt that dispatches a single tool call on a
// fresh Loop — the shape production has, one Loop per prompt — and returns the
// run's tool events.
func toolCallRunEvents(t *testing.T, prompt string) (starts []*ToolStartEvent, ends []*ToolEndEvent) {
	t.Helper()

	fp := &fakeProvider{responses: []*types.ChatResponse{
		{Choices: []types.Choice{{
			Message: types.Message{Role: "assistant", ToolCalls: []types.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: types.ToolCallFn{
					Name:      "echo",
					Arguments: `{}`,
				},
			}}},
			FinishReason: "tool_calls",
		}}},
		{Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}}},
	}}

	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "echo", result: "ok"})

	view := &recordingView{}
	loop := &Loop{
		Config:   LoopConfig{SystemPrompt: "test", MaxLoopCycles: 5},
		Provider: fp,
		Registry: reg,
		View:     view,
	}
	if _, err := loop.Run(context.Background(), prompt); err != nil {
		t.Fatalf("Run(%q) error: %v", prompt, err)
	}

	for _, ev := range view.eventsOfType() {
		switch e := ev.(type) {
		case *ToolStartEvent:
			starts = append(starts, e)
		case *ToolEndEvent:
			ends = append(ends, e)
		}
	}
	return starts, ends
}

// TestToolCallKey_TwoRunsCannotCollide pins the end-to-end property: two
// consecutive prompts, each on its own Loop, dispatch their first tool call
// with the same dispatch ordinal — and still never share a key, so the second
// prompt's end event cannot settle the first prompt's block.
func TestToolCallKey_TwoRunsCannotCollide(t *testing.T) {
	firstStarts, firstEnds := toolCallRunEvents(t, "one")
	secondStarts, secondEnds := toolCallRunEvents(t, "two")

	first := toolCallSingle(t, "first run", firstStarts, firstEnds)
	second := toolCallSingle(t, "second run", secondStarts, secondEnds)

	if first.Key == "" || second.Key == "" {
		t.Fatalf("tool events carry no key: %q and %q", first.Key, second.Key)
	}
	if first.Key == second.Key {
		t.Fatalf("both runs keyed their first call %q — a later run would settle the earlier block", first.Key)
	}
	if first.ID != second.ID {
		t.Logf("note: run ordinals differ (%d vs %d); the test asserts keys, not ordinals", first.ID, second.ID)
	}
}

// toolCallSingle asserts the run emitted exactly one paired tool call and
// returns its start event.
func toolCallSingle(t *testing.T, run string, starts []*ToolStartEvent, ends []*ToolEndEvent) *ToolStartEvent {
	t.Helper()
	if len(starts) != 1 || len(ends) != 1 {
		t.Fatalf("%s: %d start(s) and %d end(s), want one of each", run, len(starts), len(ends))
	}
	if starts[0].Key != ends[0].Key {
		t.Fatalf("%s: start key %q != end key %q", run, starts[0].Key, ends[0].Key)
	}
	return starts[0]
}

// toolCallTurnResponses is the two model turns one Run needs to dispatch a
// single tool call: the call, then the final answer.
func toolCallTurnResponses() []*types.ChatResponse {
	return []*types.ChatResponse{
		{Choices: []types.Choice{{
			Message: types.Message{Role: "assistant", ToolCalls: []types.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: types.ToolCallFn{Name: "echo", Arguments: `{}`},
			}}},
			FinishReason: "tool_calls",
		}}},
		{Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}}},
	}
}

// TestToolCallKey_OneLoopTwoRunsCannotCollide pins the reuse path: Loop.Run
// mints a fresh interaction ID on every call, so a Loop instance that outlives
// one prompt still cannot stamp two runs with the same scope (yaah-77f1).
func TestToolCallKey_OneLoopTwoRunsCannotCollide(t *testing.T) {
	fp := &fakeProvider{responses: append(toolCallTurnResponses(), toolCallTurnResponses()...)}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "echo", result: "ok"})
	view := &recordingView{}
	loop := &Loop{
		Config:   LoopConfig{SystemPrompt: "test", MaxLoopCycles: 5},
		Provider: fp,
		Registry: reg,
		View:     view,
	}

	for _, prompt := range []string{"one", "two"} {
		if _, err := loop.Run(context.Background(), prompt); err != nil {
			t.Fatalf("Run(%q) error: %v", prompt, err)
		}
	}

	var keys []string
	for _, ev := range view.eventsOfType() {
		if s, ok := ev.(*ToolStartEvent); ok {
			keys = append(keys, s.Key)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("tool starts = %d, want one per run", len(keys))
	}
	if keys[0] == keys[1] {
		t.Fatalf("one Loop reused for two prompts keyed both calls %q: reuse would collide", keys[0])
	}
	// The collision this test exists for is a shared SCOPE, not a shared key:
	// a reused Loop's dispatch ordinal keeps climbing, so equal keys would be
	// caught above even with one scope. The invariant is that every Run mints
	// its own interaction ID, so compare the scope half of each key.
	scopeOf := func(key string) string {
		if i := strings.LastIndex(key, "."); i >= 0 {
			return key[:i]
		}
		return key
	}
	if scopeOf(keys[0]) == scopeOf(keys[1]) {
		t.Fatalf("both runs share the interaction scope %q: a reused Loop would stamp one scope", scopeOf(keys[0]))
	}
}
