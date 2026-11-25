package yaah

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/tools"
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

// TestRequireReversibleWithoutSupervision pins the precedence that makes the
// key inert (yaah-ugtl review M3): require_reversible is carried unconditionally
// but every reader of it sits behind supervisedDispatch, so only an EXPLICIT
// supervisor.enabled: true gives it anything to narrow. An operator who sets it
// alone got no refusal and no diagnostic.
func TestRequireReversibleWithoutSupervision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    *bool
		requireRev bool
		want       bool
	}{
		{"neither key set", nil, false, false},
		{"require_reversible alone is inert", nil, true, true},
		{"explicit enabled: false is inert too", boolPtr(false), true, true},
		{"explicit enabled: true arms it", boolPtr(true), true, false},
		{"enabled without require_reversible", boolPtr(true), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agent.Default.Supervisor.Enabled = tc.enabled
			cfg.Agent.Default.Supervisor.RequireReversible = tc.requireRev
			if got := requireReversibleWithoutSupervision(cfg); got != tc.want {
				t.Errorf("requireReversibleWithoutSupervision = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWarnOnInertRequireReversible pins the warning itself, not only the
// predicate: the operator's only signal for the inert combination is this line.
func TestWarnOnInertRequireReversible(t *testing.T) {
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &config.Config{}
	cfg.Agent.Default.Supervisor.RequireReversible = true
	warnOnInertRequireReversible(cfg)
	if got := buf.String(); !strings.Contains(got, "has no effect without an explicit supervisor.enabled: true") {
		t.Errorf("require_reversible set alone must warn at startup, got %q", got)
	}

	// The armed posture is the documented one: nothing to say.
	buf.Reset()
	cfg.Agent.Default.Supervisor.Enabled = boolPtr(true)
	warnOnInertRequireReversible(cfg)
	if got := buf.String(); got != "" {
		t.Errorf("an armed session must not warn, got %q", got)
	}
}

// TestWireLaneRoutingMutability pins the second hook wireLaneRouting installs
// (yaah-ugtl review: the supervised lane upgrade spent a fork on read-only
// roles). The upgrade forks only where a revert has something to undo, so the
// lane a role routes to and its ability to change the workspace must be read
// from the same live registry and must agree: a role on the shared lane that
// reads as workspace-mutating is the disagreement that would either lose the
// isolation supervision promises or buy a world for a dispatch that cannot
// write.
func TestWireLaneRoutingMutability(t *testing.T) {
	st := &loopInfraStage{taskTool: &tools.DelegateTool{}}
	wireLaneRouting(st)
	if st.taskTool.LaneForRole == nil || st.taskTool.RoleMutatesWorkspace == nil {
		t.Fatal("wireLaneRouting must install both role-contract hooks")
	}
	names := []string{}
	if reg := subagent.DefaultRegistry(); reg != nil {
		names = reg.Names()
	}
	if len(names) == 0 {
		// No role registry is installed in this test binary, so there is
		// nothing to check the two hooks against. The wiring fact this test
		// exists for — both hooks installed — is already asserted above.
		t.Log("no role registry installed; the hook install is the assertion")
		return
	}
	for _, name := range names {
		role := subagent.SubAgentRole(name)
		if got, want := st.taskTool.RoleMutatesWorkspace(name), subagent.RoleMutatesWorkspace(role); got != want {
			t.Errorf("RoleMutatesWorkspace(%q) = %v, want %v", name, got, want)
		}
		switch st.taskTool.LaneForRole(name) {
		case tools.LaneShared:
			if st.taskTool.RoleMutatesWorkspace(name) {
				t.Errorf("role %q routes to the shared lane but reads as workspace-mutating", name)
			}
		case tools.LaneProposal, tools.LaneReport:
			// Isolated lanes: whether the role mutates only decides the
			// substrate the runtime already picks, not this hook.
		default:
			t.Errorf("role %q routes to unknown lane %q", name, st.taskTool.LaneForRole(name))
		}
	}
}
