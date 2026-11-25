package yaah

import (
	"testing"

	"github.com/goatherdai/yaah/internal/config"
)

// Plan 7 Phase 2: the retained lane is the default; agents.subagent.mode:
// "shared" is the explicit opt-out that keeps the legacy direct-to-host lane.
func TestRetainedMode(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"retained", true},
		{"RETAINED", true},
		{"  retained  ", true},
		{"shared", false},
		{"SHARED", false},
		{" Shared ", false},
	}
	for _, c := range cases {
		if got := retainedMode(c.in); got != c.want {
			t.Errorf("retainedMode(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRetainedLaneActive(t *testing.T) {
	for _, tc := range []struct {
		name             string
		mode             string
		retainCapable    bool
		containerSession bool
		want             bool
	}{
		{"local capable", "retained", true, false, true},
		{"local uncapturable", "retained", false, false, false},
		{"container session report lane", "retained", false, true, true},
		{"shared mode local", "shared", true, false, false},
		{"shared mode container", "shared", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retainedLaneActive(tc.mode, tc.retainCapable, tc.containerSession); got != tc.want {
				t.Errorf("retainedLaneActive(%q, %v, %v) = %v, want %v", tc.mode, tc.retainCapable, tc.containerSession, got, tc.want)
			}
		})
	}
}

// TestSupervisedDispatches pins design D5's default: only an EXPLICIT
// supervisor.enabled: true arms the supervised dispatch path. The documented
// default of the advisory rules engine is on, and reading that as "supervise
// every dispatch" would upgrade every shared-lane dispatch for sessions that
// never asked.
func TestSupervisedDispatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		en   *bool
		want bool
	}{
		{"key absent", nil, false},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agent.Default.Supervisor.Enabled = tc.en
			if got := supervisedDispatches(cfg); got != tc.want {
				t.Errorf("supervisedDispatches(%v) = %v, want %v", tc.en, got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

// TestRequireReversibleDispatches pins the config hop for
// supervisor.require_reversible: buildDelegateTool reads the key through this
// helper, so a wiring refactor that drops it fails here instead of silently
// disarming both enforcement sites (the tool's refusals and the runner start
// gate).
func TestRequireReversibleDispatches(t *testing.T) {
	cfg := &config.Config{}
	if got := requireReversibleDispatches(cfg); got {
		t.Error("requireReversibleDispatches on a zero config = true, want false")
	}
	cfg.Agent.Default.Supervisor.RequireReversible = true
	if got := requireReversibleDispatches(cfg); !got {
		t.Error("requireReversibleDispatches with the key on = false, want true")
	}
}
