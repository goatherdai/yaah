package yaah

// supervise_headless_test.go pins headless mode's composition root (yaah-0nbb,
// design §4.8): which supervisor keys reach the BatchConfig in BOTH modes (so a
// measurement and an interactive session run the same loop), when the headless
// host is built at all, and that the decider's prompt/budget pair is the one the
// paper's ~3K-token budget is comparable at.

import (
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	supervisionprompt "github.com/goatherdai/yaah/internal/prompts/supervision"
	renderer "github.com/goatherdai/yaah/internal/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// TestSuperviseBatchConfig_ConfigKeysOverride pins the other half of the
// defaults test: every supervisor key the design lists for the policy loop
// reaches the batch when the operator states it.
func TestSuperviseBatchConfig_ConfigKeysOverride(t *testing.T) {
	cfg := &config.Config{}
	sup := &cfg.Agent.Default.Supervisor
	sup.Policy = config.SupervisorPolicyHeadless
	sup.PolicyInterval = "250ms"
	sup.PolicyTrigger = config.SupervisorTriggerOnChange
	zero := 0
	sup.PolicyCooldownTicks = &zero
	sup.PolicyMaxInterventions = 7
	sup.PolicyActions = []string{config.SupervisorActionRevert, config.SupervisorActionRevert, config.SupervisorActionSteer}

	batchCfg := superviseBatchConfig(cfg, "scope:root", nil)
	if batchCfg.Interval != 250*time.Millisecond {
		t.Errorf("Interval = %v, want 250ms", batchCfg.Interval)
	}
	if batchCfg.Trigger != supervision.TriggerOnChange {
		t.Errorf("Trigger = %q, want %q", batchCfg.Trigger, supervision.TriggerOnChange)
	}
	// An EXPLICIT 0 is a configuration ("no cooldown"), not a missing key.
	if batchCfg.CooldownTicks != 0 {
		t.Errorf("CooldownTicks = %d, want the explicit 0 to survive", batchCfg.CooldownTicks)
	}
	if batchCfg.MaxInterventions != 7 {
		t.Errorf("MaxInterventions = %d, want 7", batchCfg.MaxInterventions)
	}
	if len(batchCfg.Actions) != 2 {
		t.Fatalf("Actions = %v, want the two named verbs, de-duplicated", batchCfg.Actions)
	}
	if batchCfg.Actions[0] != supervisionapi.ActionRevert || batchCfg.Actions[1] != supervisionapi.ActionSteer {
		t.Errorf("Actions = %v, want [revert steer] in the configured order", batchCfg.Actions)
	}
}

// TestSuperviseBatchConfig_EmptyActionListMasksEverything pins the one place nil
// and empty must not collapse: an absent key means all three verbs, an explicit
// empty list means none of them (the loop still ticks; it applies nothing).
func TestSuperviseBatchConfig_EmptyActionListMasksEverything(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Default.Supervisor.PolicyActions = []string{}
	mask := superviseBatchConfig(cfg, "scope:root", nil).Actions
	if mask == nil {
		t.Fatal("an explicit empty actions list must produce a NON-nil empty mask (nil is the batch's 'all three')")
	}
	if len(mask) != 0 {
		t.Errorf("Actions = %v, want an empty mask", mask)
	}
}

// TestSupervisorConfigDefaultsMatchBatchDefaults is the drift guard between the
// documented defaults config resolves and the ones the batch falls back to: two
// copies of "5s" and "one tick" that must not diverge.
func TestSupervisorConfigDefaultsMatchBatchDefaults(t *testing.T) {
	var sup config.SupervisorConfig
	if got := sup.TickInterval(); got != supervision.DefaultBatchInterval {
		t.Errorf("config's default interval = %v, want the batch's %v", got, supervision.DefaultBatchInterval)
	}
	if got := sup.CooldownTicks(); got != supervision.DefaultCooldownTicks {
		t.Errorf("config's default cooldown = %d, want the batch's %d", got, supervision.DefaultCooldownTicks)
	}
}

// TestHeadlessSnapshotBudgetReservesTheLockedPrompt pins the arithmetic the
// renderer's budget comment states: headless sends the locked system prompt AND
// the snapshot, so the snapshot's budget must leave room for the prompt's own
// ~1.4K estimate out of the paper's 3K.
func TestHeadlessSnapshotBudgetReservesTheLockedPrompt(t *testing.T) {
	if supervision.HeadlessSnapshotBudget >= renderer.DefaultRenderBudgetTokens {
		t.Fatalf("HeadlessSnapshotBudget = %d, want strictly less than the snapshot-only default %d (the system prompt is charged to the same budget)",
			supervision.HeadlessSnapshotBudget, renderer.DefaultRenderBudgetTokens)
	}
	// The reservation is chars/4 (the estimate's floor, without the 1.3 factor):
	// if even that does not fit, the pair cannot be the paper's budget at all.
	promptEstimate := len(supervisionprompt.MetaPromptV1) / 4
	if pair := supervision.HeadlessSnapshotBudget + promptEstimate; pair > renderer.DefaultRenderBudgetTokens {
		t.Errorf("system prompt (~%d) + snapshot (%d) = ~%d est tokens, over the %d-token budget the paper's loop spends",
			promptEstimate, supervision.HeadlessSnapshotBudget, pair, renderer.DefaultRenderBudgetTokens)
	}
	if supervisionprompt.MetaPromptV1 == "" {
		t.Error("the locked v1 prompt is empty")
	}
}

// TestRegisterHeadlessSupervisor_Gating pins who gets a host: only policy
// headless, and only a session that can serve it. Everything else leaves the
// field nil, which is also what the delegate hook reads — so an unconfigured or
// agent-mode session keeps its tool path byte-identical.
func TestRegisterHeadlessSupervisor_Gating(t *testing.T) {
	localProvider := map[string]config.Provider{
		"local": {BaseURL: "http://127.0.0.1:7333/v1", APIKey: "sk-test"},
	}
	cases := []struct {
		name      string
		policy    string
		model     string
		providers map[string]config.Provider
		broken    func(*loopInfraStage)
		wantNil   bool
	}{
		{name: "unconfigured session", wantNil: true},
		{name: "policy none", policy: config.SupervisorPolicyNone, wantNil: true},
		{name: "policy agent keeps the tool path", policy: config.SupervisorPolicyAgent, wantNil: true},
		{name: "headless without a model", policy: config.SupervisorPolicyHeadless, wantNil: true},
		{
			// The compaction_model spelling, exactly: a prefix that names no
			// configured provider is not an error, it is part of a bare model
			// name ("prism-ml/Bonsai-27B-gguf:Q1_0" must survive intact), so the
			// model runs on the session's default provider.
			name:   "headless with an unknown provider prefix keeps the bare model",
			policy: config.SupervisorPolicyHeadless,
			model:  "nope/some-model",
		},
		{
			// No provider at all: the loop's model cannot be resolved, so the
			// mode says so once at startup instead of supervising with a stub.
			name:      "headless with no configured provider",
			policy:    config.SupervisorPolicyHeadless,
			model:     "some-model",
			providers: map[string]config.Provider{},
			wantNil:   true,
		},
		{
			name:    "headless without a snapshot source",
			policy:  config.SupervisorPolicyHeadless,
			model:   "local/some-model",
			broken:  func(st *loopInfraStage) { st.runs = nil },
			wantNil: true,
		},
		{
			name:   "headless with a usable provider",
			policy: config.SupervisorPolicyHeadless,
			model:  "local/some-model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providers := tc.providers
			if providers == nil {
				providers = localProvider
			}
			cfg := &config.Config{Providers: providers}
			cfg.Agent.Default.Supervisor.Policy = tc.policy
			cfg.Agent.Default.Supervisor.PolicyModel = tc.model
			st := liveInfra(t)
			if tc.broken != nil {
				tc.broken(st)
			}
			registerHeadlessSupervisor(st, cfg, liveSnapshot())
			if tc.wantNil {
				if st.headless != nil {
					t.Fatal("no headless host must be built for this configuration")
				}
				return
			}
			if st.headless == nil {
				t.Fatal("policy headless with a usable model must build a host")
			}
			if st.headless.Decider == nil || st.headless.Decider.Complete == nil {
				t.Error("the host must carry a decider with a completion function")
			}
			if st.headless.Decider.Prompt != supervisionprompt.MetaPromptV1 {
				t.Error("the decider must carry the locked v1 prompt asset verbatim")
			}
			if st.headless.Decider.BudgetTokens != supervision.HeadlessSnapshotBudget {
				t.Errorf("decider budget = %d, want the headless budget %d", st.headless.Decider.BudgetTokens, supervision.HeadlessSnapshotBudget)
			}
			if st.headless.NewBatch == nil {
				t.Error("the host must build a batch per loop start")
			}
		})
	}
}

// TestResolveHeadlessPolicyModel pins the compaction_model spelling: a
// "provider/model" prefix naming a configured provider selects it, anything else
// is a bare model on the session's default provider.
func TestResolveHeadlessPolicyModel(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.Provider{
		"local":  {BaseURL: "http://127.0.0.1:7333/v1", APIKey: "sk-test"},
		"remote": {BaseURL: "https://example.invalid/v1", APIKey: "sk-remote"},
	}}
	cfg.Agent.Default.Provider = "local"

	_, model, err := resolveHeadlessPolicyModel(&config.Config{})
	if err == nil {
		t.Error("an empty policy_model must be an error, not a silent fallback")
	}
	_ = model

	cfg.Agent.Default.Supervisor.PolicyModel = "remote/sonnet-x"
	if _, got, err := resolveHeadlessPolicyModel(cfg); err != nil || got != "sonnet-x" {
		t.Errorf("remote/sonnet-x resolved to (%q, %v), want sonnet-x on the remote provider", got, err)
	}

	cfg.Agent.Default.Supervisor.PolicyModel = "gpt-local"
	if _, got, err := resolveHeadlessPolicyModel(cfg); err != nil || got != "gpt-local" {
		t.Errorf("a bare model resolved to (%q, %v), want it on the session's default provider", got, err)
	}
}
