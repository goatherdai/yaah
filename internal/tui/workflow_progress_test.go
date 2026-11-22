package tui

import (
	"testing"

	"github.com/goatherdai/yaah/internal/control"
)

// TestWorkflowProgressLine pins the compact rendering of one progress fact,
// including the run-level fallback and reason/attempt decorations.
func TestWorkflowProgressLine(t *testing.T) {
	cases := []struct {
		name string
		m    *control.WorkflowProgress
		want string
	}{
		{"run started", &control.WorkflowProgress{State: "started"}, "run started"},
		{"run finished", &control.WorkflowProgress{State: "finished"}, "run finished"},
		{"queued", &control.WorkflowProgress{NodePath: "steps.a", State: "queued"}, "steps.a queued"},
		{"running first attempt", &control.WorkflowProgress{NodePath: "steps.a", State: "running", Attempt: 1}, "steps.a running"},
		{"running retry", &control.WorkflowProgress{NodePath: "steps.a", State: "running", Attempt: 2}, "steps.a running (attempt 2)"},
		{"succeeded", &control.WorkflowProgress{NodePath: "steps.a", State: "succeeded"}, "steps.a succeeded"},
		{"failed with reason", &control.WorkflowProgress{NodePath: "steps.a", State: "failed", Reason: "boom"}, "steps.a failed: boom"},
		{"skipped", &control.WorkflowProgress{NodePath: "steps.a", State: "skipped"}, "steps.a skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowProgressLine(tc.m); got != tc.want {
				t.Errorf("workflowProgressLine(%+v) = %q, want %q", tc.m, got, tc.want)
			}
		})
	}
}
