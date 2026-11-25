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
	"slices"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
)

// liveSnapshot stands in for the session's Snapshotter: registration never
// reads it, it only proves the source is wired.
func liveSnapshot() *supervision.Snapshotter { return &supervision.Snapshotter{} }

// liveInfra returns the loopInfraStage fields registerSuperviseTool's guard
// requires beside the snapshotter: the run registry, the scope manager the
// batch binds on, and the root scope the tool is registered with as its calling
// scope. A real in-memory pair, because the guard checks them — an empty stage
// would take the degraded path (review L2-r2) — and a non-empty rootScopeID,
// because the tool refuses every call without one: begin() resolves the calling
// scope from it, so "no root scope" is its own degraded path (review r3
// finding 3) and the live fixture must not accidentally sit on it.
func liveInfra(t *testing.T) *loopInfraStage {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return &loopInfraStage{
		runs:        supervision.NewRegistry(),
		scopeMgr:    goatherd.NewScopeManager(store),
		rootScopeID: "scope:root",
	}
}

func TestSuperviseTool_RegisteredOnlyUnderAgentPolicy(t *testing.T) {
	cases := []struct {
		name        string
		policy      string
		snap        *supervision.Snapshotter
		want        bool
		wantOmitted bool
	}{
		{name: "unconfigured session", policy: "", snap: liveSnapshot(), want: false},
		{name: "policy none", policy: config.SupervisorPolicyNone, snap: liveSnapshot(), want: false},
		{name: "policy agent", policy: config.SupervisorPolicyAgent, snap: liveSnapshot(), want: true},
		// A nil snapshotter means the session's run registry or trace store
		// never came up. The tool is then NOT registered: a supervise tool
		// over a source that can never report an agent would answer every
		// tick with an empty, done batch and call it supervision. The
		// degradation is RECORDED (omittedTools, so the session's workspace
		// record and the prompt delta name it) — a policy the operator asked
		// for and did not get must not be silent.
		{name: "policy agent without a snapshot source", policy: config.SupervisorPolicyAgent, snap: nil, want: false, wantOmitted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agent.Default.Supervisor.Policy = tc.policy
			tr, err := buildToolRegistry(cfg, SessionOptions{}, &workspaceStage{cwd: t.TempDir()}, subagent.Persona{}, nil)
			if err != nil {
				t.Fatalf("buildToolRegistry: %v", err)
			}
			registerSuperviseTool(liveInfra(t), cfg, tr, tc.snap)

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
			if omitted := slices.Contains(tr.omittedTools, "supervise"); omitted != tc.wantOmitted {
				t.Errorf("supervise recorded as omitted = %v, want %v (omittedTools = %v)", omitted, tc.wantOmitted, tr.omittedTools)
			}
		})
	}
}

// TestSuperviseBatchConfig_PinsProductionDefaults pins the DEFAULTS half of
// the config → BatchConfig mapping (yaah-0nbb): an absent supervisor stanza
// must resolve to the design's defaults — the 5s interval, the one-tick
// cooldown, the always trigger, all three actions, no cap — because each would
// be a different behavior if it were zero-valued (a zero interval is a busy
// loop, a zero cooldown removes the brake design §4.9 promises, and a nil
// action mask is "all three" only by the batch's own default). The OVERRIDES
// half — an explicit stanza whose keys each beat their default, including the
// explicit `cooldown_ticks: 0` that means "no cooldown" — is pinned in
// supervise_headless_test.go, and what the batch makes of these values
// (normalization) in internal/agent/supervision's batch tests, so none of the
// three duplicates another. (Before 0nbb this comment claimed CooldownTicks
// had to be the negative default SELECTOR; that stopped being the shape when
// the config accessors took over resolution — PR #417 review round 2,
// finding 4.)
func TestSuperviseBatchConfig_PinsProductionDefaults(t *testing.T) {
	stopCalled := false
	stop := func(_ context.Context, _, _ string, _ func()) (bool, error) {
		stopCalled = true
		return true, nil
	}
	cfg := superviseBatchConfig(nil, "scope:root", stop)

	// Defaults, resolved from an ABSENT supervisor stanza. Each would be a
	// different behavior if it were zero: a zero interval is a busy loop, a zero
	// cooldown disables the brake design 4.9 promises, and a nil action slice is
	// "all three" only by the batch's own default rather than by the config's.
	if cfg.Interval != supervision.DefaultBatchInterval {
		t.Errorf("Interval = %v, want the documented default %v", cfg.Interval, supervision.DefaultBatchInterval)
	}
	if cfg.CooldownTicks != supervision.DefaultCooldownTicks {
		t.Fatalf("CooldownTicks = %d, want the documented default %d (0 would disable the cooldown outright)", cfg.CooldownTicks, supervision.DefaultCooldownTicks)
	}
	if cfg.Trigger != supervision.TriggerAlways {
		t.Errorf("Trigger = %q, want %q", cfg.Trigger, supervision.TriggerAlways)
	}
	if len(cfg.Actions) != 3 {
		t.Errorf("Actions = %v, want the three policy verbs (steer, redirect, revert)", cfg.Actions)
	}
	if cfg.MaxInterventions != 0 {
		t.Errorf("MaxInterventions = %d, want 0 (unlimited)", cfg.MaxInterventions)
	}
	if cfg.RootScopeID != "scope:root" {
		t.Errorf("RootScopeID = %q, want the caller's scope (never a member, never a target)", cfg.RootScopeID)
	}
	if cfg.Stop == nil {
		t.Fatal("Stop = nil, want the Service's stop delivery so a queued rules stop can land")
	}
	if _, err := cfg.Stop(context.Background(), "scope:sub-a", "reason", nil); err != nil || !stopCalled {
		t.Errorf("Stop did not reach the injected delivery function (called=%v, err=%v)", stopCalled, err)
	}
}

// TestNewSuperviseBatchFactory_NilServiceHasNoHooks pins the factory's
// degradation: without a supervision Service there is no rules engine to route,
// so the binding carries a batch and no arbiter hooks (and the tool simply has
// nothing to arm or release).
func TestNewSuperviseBatchFactory_NilServiceHasNoHooks(t *testing.T) {
	factory := newSuperviseBatchFactory(&loopInfraStage{}, nil, liveSnapshot())
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

// TestSuperviseTool_AdmissionWidensAnExplicitToolList pins the finding-7
// posture: under policy agent, registration goes through Belt.Admit, which
// WIDENS the session belt by the single name "supervise" even when the
// operator set an explicit agents.default.tools list that does not mention
// it. The policy stanza is itself the opt-in (like a configured MCP server);
// the explicit list still decides every other tool. Pinning the widening
// keeps a future belt refactor from silently dropping the one tool the
// operator asked for by configuring the policy.
func TestSuperviseTool_AdmissionWidensAnExplicitToolList(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Default.Supervisor.Policy = config.SupervisorPolicyAgent
	cfg.Agent.Default.Tools = []string{"read", "bash"}

	tr, err := buildToolRegistry(cfg, SessionOptions{}, &workspaceStage{cwd: t.TempDir()}, subagent.Persona{}, nil)
	if err != nil {
		t.Fatalf("buildToolRegistry: %v", err)
	}
	registerSuperviseTool(liveInfra(t), cfg, tr, liveSnapshot())

	if tr.reg.Get("supervise") == nil {
		t.Error("supervise must be registered under policy agent even when the explicit tools list omits it")
	}
	if !tr.belt.Allows("supervise") {
		t.Error("Admit must widen the explicit belt by supervise under policy agent")
	}
	if !tr.belt.Allows("read") || !tr.belt.Allows("bash") {
		t.Error("the explicit list's own tools must survive admission unchanged")
	}
}

// TestSuperviseTool_DegradedWithoutRegistryOrScopeManager pins the extended
// guard (review L2-r2, widened in r3): the batch binds on the run registry AND
// the scope manager as well as the snapshotter, and the tool is registered with
// the root scope as its caller — so a session missing any of them takes the same
// loud degraded path (tool absent, recorded as omitted) instead of registering a
// tool whose every tick reports an empty, done batch, or a tool whose every call
// fails with "no calling scope is configured".
func TestSuperviseTool_DegradedWithoutRegistryOrScopeManager(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(*loopInfraStage)
	}{
		{"no run registry", func(st *loopInfraStage) { st.runs = nil }},
		{"no scope manager", func(st *loopInfraStage) { st.scopeMgr = nil }},
		// CreateRootScope failed (ErrRootTerminal, or a degraded
		// buildGoatherdInfra): the other three inputs are present, so
		// without this arm the guard would register a supervise tool whose
		// CallerScopeID is "" and whose every call refuses. The
		// degradation must be recorded like the others.
		{"no root scope", func(st *loopInfraStage) { st.rootScopeID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Agent.Default.Supervisor.Policy = config.SupervisorPolicyAgent
			tr, err := buildToolRegistry(cfg, SessionOptions{}, &workspaceStage{cwd: t.TempDir()}, subagent.Persona{}, nil)
			if err != nil {
				t.Fatalf("buildToolRegistry: %v", err)
			}
			st := liveInfra(t)
			tc.breakIt(st)
			registerSuperviseTool(st, cfg, tr, liveSnapshot())

			if tr.reg.Get("supervise") != nil {
				t.Error("the tool must not register when a batch input or the calling scope is missing")
			}
			if !slices.Contains(tr.omittedTools, "supervise") {
				t.Errorf("the degradation must be recorded as omitted (omittedTools = %v)", tr.omittedTools)
			}
			if !tr.belt.Knows("supervise") {
				t.Error("the belt must still know the name so a persona naming supervise is not reported as a typo")
			}
		})
	}
}
