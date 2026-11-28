package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/types"
)

// A turn checkpoint is the Message JSON form, which never carries Outcome or
// Duration; the restore must copy them back from the live conversation or the
// next Rebase persists every earlier tool result as unknown and the resume
// projection falls back to reading its text (yaah-77f1).
func TestRehydrateOutcomes(t *testing.T) {
	live := []types.Message{
		types.UserMsg("q"),
		{Role: "tool", ToolCallID: "call-1", Content: "error: boom", Outcome: types.ToolOutcomeError, Duration: 1500 * time.Millisecond},
		{Role: "tool", ToolCallID: "call-legacy", Content: "error: old", Outcome: types.ToolOutcomeUnknown},
	}
	// What the checkpoint round trip actually produces: the same rows with
	// the two engine fields at zero, plus one row the live conversation no
	// longer holds.
	snap, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []types.Message
	if err := json.Unmarshal(snap, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[1].Outcome.Known() || decoded[1].Duration != 0 {
		t.Fatalf("fixture: the JSON form must drop the outcome fields, got %+v", decoded[1])
	}
	decoded = append(decoded, types.Message{Role: "tool", ToolCallID: "call-gone", Content: "error: evicted"})

	rehydrateOutcomes(decoded, live)

	if got := decoded[1].Outcome; got != types.ToolOutcomeError {
		t.Errorf("call-1 outcome = %q, want error", got)
	}
	if decoded[1].Duration != 1500*time.Millisecond {
		t.Errorf("call-1 duration = %v, want 1.5s", decoded[1].Duration)
	}
	if decoded[2].Outcome.Known() {
		t.Errorf("call-legacy outcome = %q, want unknown: a live row that recorded nothing must not invent one", decoded[2].Outcome)
	}
	if decoded[3].Outcome.Known() {
		t.Errorf("call-gone outcome = %q, want unknown: no live counterpart", decoded[3].Outcome)
	}
	if live[1].Outcome != types.ToolOutcomeError || live[1].Duration != 1500*time.Millisecond {
		t.Error("the live conversation must not be modified")
	}
}
