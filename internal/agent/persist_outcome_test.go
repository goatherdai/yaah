package agent

import (
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/types"
)

// TestSessionPersister_PersistsToolOutcome pins the write half of the
// persisted outcome (yaah-77f1): a tool result row carries the structured
// status and the measured duration, so a replay never has to infer either
// from the result text.
func TestSessionPersister_PersistsToolOutcome(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		outcome  types.ToolOutcome
		duration time.Duration
	}{
		{
			// The text of a failure carries no more information than the
			// status does; both must survive.
			name:     "error with a measured duration",
			content:  "error: exit status 1",
			outcome:  types.ToolOutcomeError,
			duration: 1500 * time.Millisecond,
		},
		{
			// A successful command whose OWN output starts with "error: "
			// persists as ok — the case the text rule got wrong.
			name:     "ok result",
			content:  "error: this is the program's own output",
			outcome:  types.ToolOutcomeOK,
			duration: 42 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newPersistTestDB(t, "sess-outcome")
			p := NewSessionPersister(db, nil, "sess-outcome")

			msg := types.ToolResultMsg("call-1", "bash", tc.content)
			msg.Outcome = tc.outcome
			msg.Duration = tc.duration
			p.Persist(msg)

			rows, err := db.GetMessages("sess-outcome")
			if err != nil {
				t.Fatalf("GetMessages: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			got := rows[0]
			if got.Outcome != string(tc.outcome) {
				t.Errorf("persisted outcome = %q, want %q", got.Outcome, tc.outcome)
			}
			if gotDur := time.Duration(got.DurationMs) * time.Millisecond; gotDur != tc.duration {
				t.Errorf("persisted duration = %v, want %v", gotDur, tc.duration)
			}
			if got.Content != tc.content || got.ToolCallID != "call-1" {
				t.Errorf("persisted content/call = %q/%q, want %q/call-1", got.Content, got.ToolCallID, tc.content)
			}
		})
	}
}

// A tool message with no recorded outcome writes the zero values, which is
// what every other (non-tool) row writes — the replay path then keeps its
// legacy rule for it rather than reading an implied success.
func TestSessionPersister_ToolOutcomeDefaultsToUnknown(t *testing.T) {
	db := newPersistTestDB(t, "sess-outcome-default")
	p := NewSessionPersister(db, nil, "sess-outcome-default")
	p.Persist(types.ToolResultMsg("call-2", "read", "file contents"))

	rows, err := db.GetMessages("sess-outcome-default")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Outcome != "" || rows[0].DurationMs != 0 {
		t.Errorf("unrecorded row = outcome %q duration %d, want empty/0", rows[0].Outcome, rows[0].DurationMs)
	}
}
