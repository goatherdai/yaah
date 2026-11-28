package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// TestToolEndEventCarriesOutcome drives a tool through the loop and asserts
// the published end event reports a structured status, so a consumer settles
// a block from data instead of pattern-matching Result (yaah-77f1).
func TestToolEndEventCarriesOutcome(t *testing.T) {
	tests := []struct {
		name      string
		toolErr   error
		want      types.ToolOutcome
		wantError bool
	}{
		{name: "failing tool", toolErr: errors.New("exit status 1"), want: types.ToolOutcomeError, wantError: true},
		{name: "successful tool", want: types.ToolOutcomeOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{responses: toolCallTurnResponses()}
			reg := tools.NewRegistry()
			reg.Register(&fakeTool{name: "echo", result: "ok", err: tc.toolErr})
			view := &recordingView{}
			loop := &Loop{
				Config:   LoopConfig{SystemPrompt: "test", MaxLoopCycles: 5},
				Provider: fp,
				Registry: reg,
				View:     view,
			}
			if _, err := loop.Run(context.Background(), "go"); err != nil {
				t.Fatalf("Run error: %v", err)
			}

			var end *ToolEndEvent
			for _, ev := range view.eventsOfType() {
				if e, ok := ev.(*ToolEndEvent); ok {
					end = e
				}
			}
			if end == nil {
				t.Fatal("no ToolEndEvent published")
			}
			if end.Outcome != tc.want {
				t.Errorf("ToolEndEvent.Outcome = %q, want %q", end.Outcome, tc.want)
			}
			if gotErr := end.Error != ""; gotErr != tc.wantError {
				t.Errorf("ToolEndEvent.Error = %q, want error present = %v", end.Error, tc.wantError)
			}
			// The additive fields must describe the untruncated result: a
			// small result is never reported as truncated, and the byte
			// count is what the consumer would have seen in full.
			if end.Truncated {
				t.Errorf("small result reported as truncated (ResultBytes %d, Result %d bytes)", end.ResultBytes, len(end.Result))
			}
			if end.ResultBytes != len(end.Result) {
				t.Errorf("ResultBytes = %d, want %d (the untruncated result)", end.ResultBytes, len(end.Result))
			}
			if end.Duration <= 0 {
				t.Errorf("Duration = %v, want the measured execution time", end.Duration)
			}
		})
	}
}
