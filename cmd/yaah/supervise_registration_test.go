package yaah

// supervise_registration_test.go pins the supervise tool's registration gate
// (yaah-fy28, design §5 "default posture"): under policy none — the default,
// including an unconfigured session — no supervise tool is registered and no
// batch exists; under policy agent it is registered and belt-admitted.
//
// It also pins the PRODUCTION batch config and the factory's arbiter hooks, so
// the settings the composition root builds cannot drift away from the ones the
// tool's doc and design §4.9 promise.

import (
	"context"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
)

// liveSnapshot stands in for the session's Snapshotter: registration never
// reads it, it only proves the source is wired.
func liveSnapshot() *supervision.Snapshotter { return &supervision.Snapshotter{} }

func TestSuperviseTool_RegisteredOnlyUnderAgentPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		snap   *supervision.Snapshotter
		want   bool
	}{
		{name: "unconfigured session", policy: "", snap: liveSnapshot(), want: false},
		{name: "policy none", policy: config.SupervisorPolicyNone, snap: liveSnapshot(), want: false},
		{name: "policy agent", policy: config.SupervisorPolicyAgent, snap: liveSnapshot(), want: true},
		// A nil snapshotter means the session's run registry or trace store
		// never came up. The tool is then NOT registered: a supervise tool
		// over a source that can never report an agent would answer every
		// tick with an empty, done batch and call it supervision.
		{name: "policy agent without a snapshot source", policy: config.SupervisorPolicyAgent, snap: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agent.Default.Supervisor.Policy = tc.policy
			tr, err := buildToolRegistry(cfg, SessionOptions{}, &workspaceStage{cwd: t.TempDir()}, subagent.Persona{}, nil)
			if err != nil {
				t.Fatalf("buildToolRegistry: %v", err)
			}
			registerSuperviseTool(&loopInfraStage{}, cfg, tr, tc.snap)

			registered := tr.reg.Get("supervise") != nil
			if registered != tc.want {
				t.Fatalf("registered = %v, want %v (policy %q)", registered, tc.want, tc.policy)
			}
			if !tr.belt.Knows("supervise") {
				t.Error("the belt must know the name so a persona naming supervise is not reported as a typo")
			}
			if tc.want && !tr.belt.Allows("supervise") {
				t.Error("policy agent must belt-admit supervise")
			}
			if !tc.want && tr.belt.Allows("supervise") {
				t.Error("under policy none the belt must not admit supervise")
			}
		})
	}
}

// TestSuperviseBatchConfig_PinsProductionDefaults pins the BatchConfig the
// production factory builds. CooldownTicks must be the design's DEFAULT
// SELECTOR (negative), never zero: zero disables the cooldown outright, so a
// zero-valued field here would silently drop the `cooldown_ticks: 1` brake that
// design §4.9 and the tool's doc both promise. supervision tests pin that a
// negative value normalizes to supervision.DefaultCooldownTicks.
func TestSuperviseBatchConfig_PinsProductionDefaults(t *testing.T) {
	stopCalled := false
	stop := func(_ context.Context, _, _ string) (bool, error) {
		stopCalled = true
		return true, nil
	}
	cfg := superviseBatchConfig("scope:root", stop)

	if cfg.CooldownTicks != -1 {
		t.Errorf("CooldownTicks = %d, want -1 (the documented default selector; 0 would disable the cooldown)", cfg.CooldownTicks)
	}
	if cfg.CooldownTicks >= 0 {
		t.Fatalf("CooldownTicks = %d: the production batch would run with no cooldown", cfg.CooldownTicks)
	}
	if cfg.RootScopeID != "scope:root" {
		t.Errorf("RootScopeID = %q, want the caller's scope (never a member, never a target)", cfg.RootScopeID)
	}
	if cfg.Stop == nil {
		t.Fatal("Stop = nil, want the Service's stop delivery so a queued rules stop can land")
	}
	if _, err := cfg.Stop(context.Background(), "scope:sub-a", "reason"); err != nil || !stopCalled {
		t.Errorf("Stop did not reach the injected delivery function (called=%v, err=%v)", stopCalled, err)
	}
}

// TestNewSuperviseBatchFactory_NilServiceHasNoHooks pins the factory's
// degradation: without a supervision Service there is no rules engine to route,
// so the binding carries a batch and no arbiter hooks (and the tool simply has
// nothing to arm or release).
func TestNewSuperviseBatchFactory_NilServiceHasNoHooks(t *testing.T) {
	factory := newSuperviseBatchFactory(&loopInfraStage{}, liveSnapshot())
	binding := factory("scope:root")
	if binding == nil {
		t.Fatal("factory returned nil")
	}
	if binding.Batch == nil {
		t.Fatal("binding has no batch")
	}
	if binding.Arm != nil || binding.Release != nil {
		t.Errorf("Arm/Release = %v/%v, want nil with no supervision service wired", binding.Arm != nil, binding.Release != nil)
	}
}
