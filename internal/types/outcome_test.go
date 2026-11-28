package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// The zero value must mean UNKNOWN, not ok: a row written before outcomes
// were persisted carries nothing, and treating "nothing" as success is the
// misclassification this type exists to remove (yaah-77f1, yaah-bmv3).
func TestToolOutcomeZeroValueIsUnknown(t *testing.T) {
	var zero ToolOutcome
	if zero != ToolOutcomeUnknown {
		t.Fatalf("zero ToolOutcome = %q, want the unknown constant", zero)
	}
	if zero.Known() {
		t.Error("the zero outcome reports itself as known")
	}
	if !ToolOutcomeOK.Known() || !ToolOutcomeError.Known() ||
		!ToolOutcomeCancelled.Known() || !ToolOutcomeInterrupted.Known() {
		t.Error("a defined outcome reports itself as unknown")
	}
}

// A synthesized result can only be built with its status: ToolFailedMsg is
// the constructor the middleware use, so a row for a call that never ran
// cannot persist as unknown (yaah-77f1). An executed result stays unknown
// here on purpose — the dispatch path classifies it from the tool's error.
func TestToolFailedMsgCarriesErrorOutcome(t *testing.T) {
	msg := ToolFailedMsg("call-1", "bash", "error: denied")
	if msg.Role != "tool" || msg.ToolCallID != "call-1" || msg.Name != "bash" || msg.Content != "error: denied" {
		t.Errorf("ToolFailedMsg = %+v, want a tool result for call-1/bash", msg)
	}
	if msg.Outcome != ToolOutcomeError {
		t.Errorf("Outcome = %q, want %q", msg.Outcome, ToolOutcomeError)
	}
	if plain := ToolResultMsg("call-1", "bash", "x"); plain.Outcome != ToolOutcomeUnknown {
		t.Errorf("ToolResultMsg.Outcome = %q, want unknown", plain.Outcome)
	}
}

func TestParseToolOutcome(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ToolOutcome
	}{
		{"unknown empty", "", ToolOutcomeUnknown},
		{"ok", "ok", ToolOutcomeOK},
		{"error", "error", ToolOutcomeError},
		{"cancelled", "cancelled", ToolOutcomeCancelled},
		{"interrupted", "interrupted", ToolOutcomeInterrupted},
		// A value this binary does not define — a row written by a newer
		// version, or garbage — must degrade to unknown rather than be
		// trusted as a status nobody implemented.
		{"unrecognized", "timed_out", ToolOutcomeUnknown},
		{"case is significant", "OK", ToolOutcomeUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseToolOutcome(tc.in); got != tc.want {
				t.Errorf("ParseToolOutcome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The message fields are engine bookkeeping, never provider wire data: a
// provider must never receive them (and a prompt-cache digest over the wire
// form must not move because an outcome was recorded).
func TestMessageOutcomeFieldsAreNotMarshalled(t *testing.T) {
	data, err := json.Marshal(Message{Role: "tool", Content: "out", Outcome: ToolOutcomeError, Duration: 1500})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, unwanted := range []string{"outcome", "duration", "error"} {
		if strings.Contains(string(data), unwanted) {
			t.Errorf("marshalled message leaked %q: %s", unwanted, data)
		}
	}
}
