package jobs

import (
	"context"
	"testing"
)

// TestSubAgentDenialsRoundTrip: a dispatch body installs a sink, the runner
// writes the refusals its guard made through it, and the body reads them back.
func TestSubAgentDenialsRoundTrip(t *testing.T) {
	var got []SubAgentDenial
	ctx := WithSubAgentDenials(context.Background(), &got)

	want := []SubAgentDenial{
		{Tool: "bash", Command: "rm -rf /", Reason: "Blocked destructive operation."},
		{Tool: "bash", Command: "git status", Reason: "not in this role's allow_commands allowlist", Rule: "role_shell_allowlist"},
	}
	CaptureSubAgentDenials(ctx, want)

	if len(got) != len(want) {
		t.Fatalf("captured %d denials, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("captured[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCaptureSubAgentDenialsNoSink: capture is a no-op, not a panic, when the
// caller installed no sink.
func TestCaptureSubAgentDenialsNoSink(t *testing.T) {
	CaptureSubAgentDenials(context.Background(), []SubAgentDenial{{Tool: "bash"}})
}
