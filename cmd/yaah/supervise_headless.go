package yaah

// supervise_headless.go is headless mode's composition root (yaah-0nbb, design
// §4.8): policy: headless runs the meta-agent as a HOST loop instead of as the
// dispatching agent's own turn cycle. One stateless JSON-mode completion per
// tick over the rendered snapshot, applied through the same Batch the `supervise`
// tool drives — no conversation, no tools, no settlement authority (§4.12).
//
// What is assembled here, and why here:
//
//   - the batch's inputs are the ones the agent-mode tool binds (the session's
//     run registry, the descendant-filtered snapshot source, the Service's stop
//     delivery), so the two modes are the same loop with a different decider —
//     which is the whole point of the measurement (yaah-1050.1.5);
//   - the model comes from supervisor.policy_model, resolved the way
//     compaction_model is (a "provider/model" prefix when the prefix names a
//     configured provider, else the bare model on the session's default
//     provider), and it is ONE completion call with a JSON response format: the
//     decider can call nothing else;
//   - the system prompt is the byte-pinned v1 asset, loaded from the asset
//     package that owns it (internal/prompts/supervision), never from a string
//     literal here — the drift test pins those bytes and a measurement is only
//     comparable to the paper's if they are the ones it sends;
//   - the rules arbiter is armed and released by the host (HeadlessHost.Arm /
//     Release), exactly as the `supervise` tool's binding does, so one arbiter
//     serves rules and decisions in both modes (§4.6).
//
// Headless registers NO tool: registerSuperviseTool only registers under
// PolicyAgent, so the two modes cannot both be live.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/agent/llm"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	supervisionprompt "github.com/goatherdai/yaah/internal/prompts/supervision"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// registerHeadlessSupervisor builds the session's headless loop host, and only
// under policy: headless. It is the same shape as registerSuperviseTool: the
// policy stanza is the opt-in, a session that cannot serve it says so ONCE at
// startup (rather than silently supervising nothing), and under every other
// policy nothing is constructed at all.
func registerHeadlessSupervisor(st *loopInfraStage, cfg *config.Config, snap *supervision.Snapshotter) {
	if !cfg.Agent.Default.Supervisor.PolicyHeadless() {
		return
	}
	mode := cfg.Agent.Default.Supervisor.PolicyMode()
	// The same four inputs the agent-mode batch binds, for the same reasons
	// (registerSuperviseTool's guard explains each): without the snapshotter
	// there is nothing to render, without the registry no claim or steer can
	// land, without the scope manager the descendant filter sees no tree, and
	// without the root scope there is no membership anchor — every one of them
	// would produce a loop that ticks over an empty batch and calls it
	// supervision.
	if snap == nil || st.runs == nil || st.scopeMgr == nil || st.rootScopeID == "" {
		fmt.Fprintf(os.Stderr, "warning: supervisor.policy is %q but this session has no supervision target (the run registry, the scope manager, the trace store, or the root scope did not start), so no headless supervision loop will run\n", mode)
		return
	}
	provider, model, err := resolveHeadlessPolicyModel(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: supervisor.policy is %q but %v — no headless supervision loop will run\n", mode, err)
		return
	}
	batchCfg := superviseBatchConfig(cfg, st.rootScopeID, superviseStopFunc(st.supervisorSvc))
	host := &supervision.HeadlessHost{
		// One batch per loop START, built late: a restarted loop begins with a
		// clean tick counter, label map and claim table, exactly as agent mode's
		// batch is built on first use rather than at startup.
		NewBatch: func() *supervision.Batch {
			return supervision.NewBatch(st.runs, &tools.DescendantSnapshot{
				Source: snap,
				Scopes: st.scopeMgr,
				Caller: st.rootScopeID,
			}, batchCfg)
		},
		Decider: &supervision.HeadlessDecider{
			Complete: headlessCompletion(provider, model),
			// The locked asset, selected by supervisor.prompt (v1 is the only
			// version that exists; validation refuses anything else).
			Prompt: supervisionprompt.MetaPromptV1,
			// The snapshot gets the paper's ~3K budget MINUS the system prompt's
			// own estimate, so the pair is the budget the paper measured at.
			BudgetTokens: supervision.HeadlessSnapshotBudget,
			Log:          slog.Default(),
		},
		Log: slog.Default(),
	}
	if svc := st.supervisorSvc; svc != nil {
		// One arbiter for rules and decisions (design §4.6), the same pairing
		// the `supervise` tool's binding arms: while the loop runs, a rule firing
		// queues for its next tick instead of being delivered beside it, and
		// Release on stop puts the engine back on direct delivery so a stopped
		// loop cannot swallow the session's firings.
		host.Arm = func(a supervision.RuleArbiter) { svc.SetArbiter(a) }
		host.Release = func() { svc.SetArbiter(nil) }
	}
	st.headless = host
}

// resolveHeadlessPolicyModel turns supervisor.policy_model into a usable
// provider and model. The spelling is compaction_model's, deliberately — one
// convention for "which model runs this side job" — so a value the operator
// already knows how to write works here: "provider/model" when the prefix names
// a configured provider, otherwise the bare model on the session's default
// provider.
//
// A model that resolves to nothing usable is an error the caller reports once at
// startup, not a silent fallback to the session's own model: the decider's model
// is a measurement parameter (design §4.9 requires the key for headless), and
// substituting another one would produce numbers nobody can attribute.
func resolveHeadlessPolicyModel(cfg *config.Config) (agent.Provider, string, error) {
	name := cfg.Agent.Default.Supervisor.PolicyModelName()
	if name == "" {
		return nil, "", fmt.Errorf("supervisor.policy_model is empty, and the headless decider is one model call per tick")
	}
	providerName, model := resolveProviderName(cfg), name
	if parts := strings.SplitN(name, "/", 2); len(parts) == 2 {
		if strings.TrimSpace(parts[1]) == "" {
			// The "provider/" spelling with an empty model half would make
			// EVERY tick's completion fail (PR #417 review round 2, finding 7):
			// refused once here, naming the key, instead of arming a loop that
			// can never decide.
			return nil, "", fmt.Errorf("supervisor.policy_model %q names no model after its provider prefix: use \"provider/model\" or a bare model on the session's default provider", name)
		}
		if _, ok := cfg.Providers[parts[0]]; ok {
			providerName, model = parts[0], parts[1]
		}
	}
	if p, ok := cfg.Providers[providerName]; ok {
		if prov, usable := makeProvider(providerName, p); usable {
			return prov, model, nil
		}
		return nil, "", fmt.Errorf("supervisor.policy_model %q names provider %q, which has no usable credential or endpoint", name, providerName)
	}
	return nil, "", fmt.Errorf("supervisor.policy_model %q names provider %q, which is not configured", name, providerName)
}

// headlessCompletion adapts a provider to the decider's one-shot call: one
// JSON-mode completion with the locked system prompt and the rendered snapshot,
// and nothing else (design §4.8, §4.12). No tools are advertised, no history is
// replayed, and no usage is attributed to the session — the decider is not a
// worker, and a measurement's cost belongs to the loop, not to the dispatch it
// supervised.
func headlessCompletion(provider agent.Provider, model string) supervision.CompletionFunc {
	client := &llm.Client{
		Provider: provider,
		Model:    model,
		// One retry: a tick is not worth a retry ladder, and the next tick is a
		// fresh call against a fresh snapshot.
		MaxRetries: 1,
	}
	return func(ctx context.Context, system, snapshot string) (string, error) {
		res, err := client.Call(ctx, types.ChatRequest{
			Model: model,
			Messages: []types.Message{
				{Role: "system", Content: system},
				{Role: "user", Content: snapshot},
			},
			// The renderer's footer instruction asks for one JSON object; JSON
			// mode is what makes that a provider-enforced shape rather than a
			// request the model may ignore.
			ResponseFormat: &types.ResponseFormat{Type: "json_object"},
		})
		if err != nil {
			return "", err
		}
		return res.Message.Content, nil
	}
}
