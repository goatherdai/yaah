package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentctx "github.com/goatherdai/yaah/internal/agent/context"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// TestTruncateToolResultFlagIsTheCuttersVerdict pins that the truncation
// flag comes from the cutter, not from a length comparison: a result cut
// only by the line cap comes back LONGER than the original once the notice
// is appended, so a "shorter than before" heuristic reports false exactly
// when truncation happened (yaah-77f1).
func TestTruncateToolResultFlagIsTheCuttersVerdict(t *testing.T) {
	const maxLines, maxBytes = 500, 20 * 1024
	lineCapped := strings.Repeat(strings.Repeat("x", 39)+"\n", maxLines+1)
	byteCapped := strings.Repeat(strings.Repeat("y", 2999)+"\n", 10)
	if len(lineCapped) > maxBytes {
		t.Fatalf("fixture: line-capped result must stay under the byte cap (%d bytes)", len(lineCapped))
	}
	if len(byteCapped) <= maxBytes {
		t.Fatalf("fixture: byte-capped result must exceed the byte cap (%d bytes)", len(byteCapped))
	}
	tests := []struct {
		name   string
		result string
		cut    bool
	}{
		{"fits: identity, not cut", "small\nresult", false},
		{"line cap only: longer than the original, still cut", lineCapped, true},
		{"byte cap only", byteCapped, true},
	}
	loop := &Loop{Config: LoopConfig{SystemPrompt: "test", MaxLoopCycles: 5}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, cut := loop.truncateToolResultFlag(tc.result)
			if cut != tc.cut {
				t.Errorf("cut = %v, want %v (in %d bytes, out %d bytes)", cut, tc.cut, len(tc.result), len(out))
			}
			if !tc.cut && out != tc.result {
				t.Errorf("an uncut result must be returned unchanged")
			}
			if tc.cut && !strings.Contains(out, "truncated") {
				t.Errorf("a cut result must carry the truncation notice: %q", out[len(out)-200:])
			}
		})
	}
	if cut := len(lineCapped) < len(mustCut(t, loop, lineCapped)); !cut {
		t.Errorf("fixture: the line-capped result must come back LONGER than the original for this test to kill a length heuristic")
	}
}

func mustCut(t *testing.T, loop *Loop, in string) string {
	t.Helper()
	out, _ := loop.truncateToolResultFlag(in)
	return out
}

// TestToolEndEventReportsTruncation drives a line-capped result through the
// loop and asserts the published end event says so, with ResultBytes naming
// the untruncated size (yaah-77f1).
func TestToolEndEventReportsTruncation(t *testing.T) {
	// 501 lines of 16 bytes: over the 500-line cap, under BOTH the loop's
	// 20 KiB byte cap and the tool runner's 8 KiB success cap (which would
	// otherwise cut it to ~500 lines before the loop ever saw it), so only
	// the loop's line cap fires.
	big := strings.Repeat(strings.Repeat("x", 15)+"\n", agentctx.DefaultTruncateMaxLines+1)
	fp := &fakeProvider{responses: toolCallTurnResponses()}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "echo", result: big})
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
	if !end.Truncated {
		t.Errorf("Truncated = false for a line-capped result (ResultBytes %d, emitted %d bytes)", end.ResultBytes, len(end.Result))
	}
	if end.ResultBytes != len(big) {
		t.Errorf("ResultBytes = %d, want the untruncated size %d", end.ResultBytes, len(big))
	}
}

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
			// The delay must exceed the coarsest clock tick any CI host
			// reports: Windows' interrupt-time clock can advance as
			// little as once per ~15.6ms, so an instantly-returning
			// fake measures Duration == 0 there and trips the
			// measured-execution-time assertion below (yaah-9pch).
			reg.Register(&fakeTool{name: "echo", result: "ok", err: tc.toolErr, delay: 20 * time.Millisecond})
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
