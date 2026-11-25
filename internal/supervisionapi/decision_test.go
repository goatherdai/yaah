package supervisionapi

import (
	"context"
	"testing"
)

// TestWordCount pins the single rule both limit checkers will call
// (yaah-3l34, yaah-0nbb): whitespace-separated tokens, unicode whitespace,
// so an empty or all-space string is 0.
func TestWordCount(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"spaces only", "   ", 0},
		{"one word", "retry", 1},
		{"several words", "the agent is looping on the same read", 8},
		{"repeated spaces", "two  words", 2},
		{"newlines", "one\ntwo\n\nthree", 3},
		{"tabs", "one\ttwo\t\tthree", 3},
		{"non-ASCII space", "un\u00a0deux\u2003trois", 3},
		{"surrounding whitespace", "  leading and trailing  ", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WordCount(tt.in); got != tt.want {
				t.Errorf("WordCount(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestActionValues pins the wire form: the supervise tool accepts these
// strings and the envelope records them, so later beads (renderer,
// headless decider) can match on the literals.
func TestActionValues(t *testing.T) {
	tests := []struct {
		name string
		got  Action
		want string
	}{
		{"none", ActionNone, "none"},
		{"steer", ActionSteer, "steer"},
		{"redirect", ActionRedirect, "redirect"},
		{"revert", ActionRevert, "revert"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.got) != tt.want {
				t.Errorf("Action %s = %q, want %q", tt.name, string(tt.got), tt.want)
			}
		})
	}
}

// TestSourceAndOutcomeValues pins the record vocabulary: the intervention
// record's Source and Outcome fields carry exactly these strings.
func TestSourceAndOutcomeValues(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  string
		want string
	}{
		{"source policy", SourcePolicy, "policy"},
		{"source rules", SourceRules, "rules"},
		{"source root", SourceRoot, "root"},
		{"outcome applied", OutcomeApplied, "applied"},
		{"outcome claimed", OutcomeClaimed, "claimed"},
		{"outcome dropped", OutcomeDropped, "dropped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

// TestDeciderSeal pins the seam the later beads implement: a Decider is
// any type that turns one snapshot into one list of decisions.
func TestDeciderSeal(t *testing.T) {
	var _ Decider = deciderFunc(func(_ context.Context, _ Snapshot) ([]Decision, error) {
		return nil, nil
	})
}

type deciderFunc func(ctx context.Context, snap Snapshot) ([]Decision, error)

func (f deciderFunc) Decide(ctx context.Context, snap Snapshot) ([]Decision, error) {
	return f(ctx, snap)
}
