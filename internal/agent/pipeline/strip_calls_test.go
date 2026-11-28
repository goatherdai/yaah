package pipeline

import (
	"reflect"
	"testing"

	"github.com/goatherdai/yaah/internal/types"
)

func stripTestCall(id, name string) types.ToolCall {
	return types.ToolCall{ID: id, Function: types.ToolCallFn{Name: name, Arguments: `{}`}}
}

func stripTestIDs(calls []types.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, tc := range calls {
		ids = append(ids, tc.ID)
	}
	return ids
}

func TestStripCalls(t *testing.T) {
	calls := []types.ToolCall{
		stripTestCall("a", "read"),
		stripTestCall("b", "edit"),
		stripTestCall("c", "bash"),
	}
	keepAll := func(int, types.ToolCall) stripDecision { return stripDecision{keep: true} }
	stripAll := func(int, types.ToolCall) stripDecision { return stripDecision{reason: "denied"} }

	t.Run("all kept", func(t *testing.T) {
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls...)}
		step := &Step{}
		hooks := 0
		if got := stripCalls(msg, step, keepAll, func(types.ToolCall, string) { hooks++ }); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
		if want := []string{"a", "b", "c"}; !reflect.DeepEqual(stripTestIDs(msg.ToolCalls), want) {
			t.Fatalf("kept = %v, want %v", stripTestIDs(msg.ToolCalls), want)
		}
		if len(step.SynthesizedResults) != 0 || hooks != 0 {
			t.Fatalf("synthesized %d results, fired %d hooks", len(step.SynthesizedResults), hooks)
		}
	})

	t.Run("all stripped", func(t *testing.T) {
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls...)}
		step := &Step{}
		var hooked []string
		if got := stripCalls(msg, step, stripAll, func(tc types.ToolCall, reason string) {
			if reason != "denied" {
				t.Errorf("hook reason = %q, want denied", reason)
			}
			hooked = append(hooked, tc.ID)
		}); got != 3 {
			t.Fatalf("dropped = %d, want 3", got)
		}
		if len(msg.ToolCalls) != 0 {
			t.Fatalf("kept = %v, want none", stripTestIDs(msg.ToolCalls))
		}
		if len(step.SynthesizedResults) != 3 {
			t.Fatalf("synthesized %d results, want 3", len(step.SynthesizedResults))
		}
		for i, wantID := range []string{"a", "b", "c"} {
			got := step.SynthesizedResults[i]
			if got.ToolCallID != wantID || got.Content != "denied" || got.Outcome != types.ToolOutcomeError {
				t.Errorf("result %d = %+v, want error result for %s", i, got, wantID)
			}
		}
		if !reflect.DeepEqual(hooked, []string{"a", "b", "c"}) {
			t.Fatalf("hooks = %v, want a,b,c", hooked)
		}
	})

	t.Run("mixed keeps order", func(t *testing.T) {
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls...)}
		step := &Step{}
		got := stripCalls(msg, step, func(_ int, tc types.ToolCall) stripDecision {
			if tc.ID == "b" {
				return stripDecision{reason: "no"}
			}
			return stripDecision{keep: true}
		}, nil)
		if got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}
		if want := []string{"a", "c"}; !reflect.DeepEqual(stripTestIDs(msg.ToolCalls), want) {
			t.Fatalf("kept = %v, want %v", stripTestIDs(msg.ToolCalls), want)
		}
		if len(step.SynthesizedResults) != 1 || step.SynthesizedResults[0].ToolCallID != "b" {
			t.Fatalf("results = %+v, want one for b", step.SynthesizedResults)
		}
	})

	t.Run("index predicate", func(t *testing.T) {
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls...)}
		step := &Step{}
		got := stripCalls(msg, step, func(i int, _ types.ToolCall) stripDecision {
			if i < 2 {
				return stripDecision{keep: true}
			}
			return stripDecision{reason: "over limit"}
		}, nil)
		if got != 1 || !reflect.DeepEqual(stripTestIDs(msg.ToolCalls), []string{"a", "b"}) {
			t.Fatalf("dropped = %d, kept = %v", got, stripTestIDs(msg.ToolCalls))
		}
	})

	t.Run("empty reason still strips", func(t *testing.T) {
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls[:1]...)}
		step := &Step{}
		if got := stripCalls(msg, step, func(int, types.ToolCall) stripDecision { return stripDecision{} }, nil); got != 1 {
			t.Fatalf("dropped = %d, want 1", got)
		}
		if len(msg.ToolCalls) != 0 || len(step.SynthesizedResults) != 1 || step.SynthesizedResults[0].Content != "" {
			t.Fatalf("empty reason must still strip: kept=%v results=%+v", stripTestIDs(msg.ToolCalls), step.SynthesizedResults)
		}
	})

	t.Run("does not alias input slice", func(t *testing.T) {
		original := append([]types.ToolCall(nil), calls...)
		msg := &types.Message{ToolCalls: append([]types.ToolCall(nil), calls...)}
		step := &Step{}
		stripCalls(msg, step, func(_ int, tc types.ToolCall) stripDecision {
			if tc.ID == "a" {
				return stripDecision{keep: true}
			}
			return stripDecision{reason: "no"}
		}, nil)
		msg.ToolCalls[0].ID = "mutated"
		if original[0].ID != "a" || original[1].ID != "b" || original[2].ID != "c" {
			t.Fatalf("input slice aliased: %v", stripTestIDs(original))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		msg := &types.Message{}
		if got := stripCalls(msg, &Step{}, keepAll, nil); got != 0 {
			t.Fatalf("dropped = %d, want 0", got)
		}
	})
}
