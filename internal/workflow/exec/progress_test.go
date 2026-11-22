package exec

import (
	"strings"
	"testing"
)

// Tests for the live-progress seam (workflow-in-session Scope B): the
// ProgressEvent projection and the Config.Progress callback that delivers it.

func TestProjectProgress(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want ProgressEvent
	}{
		{
			name: "run started has no node",
			ev:   RunStartedEvent{RunID: "r1"},
			want: ProgressEvent{Kind: EventRunStarted},
		},
		{
			name: "node ready",
			ev:   NodeReadyEvent{NodePath: "steps.a"},
			want: ProgressEvent{Kind: EventNodeReady, NodePath: "steps.a"},
		},
		{
			name: "dispatch started carries attempt",
			ev:   DispatchStartedEvent{Attempt: &Attempt{Number: 2, NodePath: "steps.a"}},
			want: ProgressEvent{Kind: EventDispatchStarted, NodePath: "steps.a", Attempt: 2},
		},
		{
			name: "node succeeded",
			ev:   NodeSucceededEvent{NodePath: "steps.a"},
			want: ProgressEvent{Kind: EventNodeSucceeded, NodePath: "steps.a"},
		},
		{
			name: "node failed carries reason",
			ev:   NodeFailedEvent{NodePath: "steps.a", Reason: "boom"},
			want: ProgressEvent{Kind: EventNodeFailed, NodePath: "steps.a", Reason: "boom"},
		},
		{
			name: "node skipped carries reason",
			ev:   NodeSkippedEvent{NodePath: "steps.a", Reason: "guard"},
			want: ProgressEvent{Kind: EventNodeSkipped, NodePath: "steps.a", Reason: "guard"},
		},
		{
			name: "node cancelled carries reason",
			ev:   NodeCancelledEvent{NodePath: "steps.a", Reason: "halt"},
			want: ProgressEvent{Kind: EventNodeCancelled, NodePath: "steps.a", Reason: "halt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectProgress(tc.ev); got != tc.want {
				t.Errorf("projectProgress(%T) = %+v, want %+v", tc.ev, got, tc.want)
			}
		})
	}
}

// TestProgressSeamFiresForDerivedFacts pins that the callback reaches both
// reported and derived events: dispatch_started is submitted by the driver,
// but node_ready and node_succeeded are derived by the reducer, so their
// presence is the regression guard for a seam that only listened on Apply.
func TestProgressSeamFiresForDerivedFacts(t *testing.T) {
	y := strings.Replace(chainYAML, "%s", "chain", 1)
	g := graphFromYAML(t, y)

	var got []ProgressEvent
	_, _ = runGraph(t, g, okScript("steps.a", "steps.b", "steps.c"), Config{
		RunID: "progress-1",
		Progress: func(p ProgressEvent) {
			got = append(got, p)
		},
	})

	if len(got) == 0 {
		t.Fatal("no progress events delivered")
	}
	if got[len(got)-1].Kind != EventRunFinished {
		t.Errorf("last progress event = %s, want run_finished", got[len(got)-1].Kind)
	}

	counts := map[string]int{}
	for _, p := range got {
		counts[string(p.Kind)+"/"+p.NodePath]++
	}
	if counts[string(EventRunStarted)+"/"] != 1 {
		t.Errorf("run_started seen %d times, want 1", counts[string(EventRunStarted)+"/"])
	}
	for _, path := range []string{"steps.a", "steps.b", "steps.c"} {
		if counts[string(EventNodeReady)+"/"+path] != 1 {
			t.Errorf("node_ready for %s seen %d times, want 1 (derived fact must reach the seam)",
				path, counts[string(EventNodeReady)+"/"+path])
		}
		if counts[string(EventDispatchStarted)+"/"+path] != 1 {
			t.Errorf("dispatch_started for %s seen %d times, want 1",
				path, counts[string(EventDispatchStarted)+"/"+path])
		}
		if counts[string(EventNodeSucceeded)+"/"+path] != 1 {
			t.Errorf("node_succeeded for %s seen %d times, want 1 (derived fact must reach the seam)",
				path, counts[string(EventNodeSucceeded)+"/"+path])
		}
	}
}
