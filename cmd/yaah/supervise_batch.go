package yaah

// supervise_batch.go is the `supervise` tool's composition root (yaah-fy28,
// design §4.7). The tool lives in internal/tools and knows nothing about the
// agent family; everything a tick touches is assembled here, once, and handed
// over as one factory closure:
//
//   - the session's run registry, which the arbiter steers and claims through;
//   - the snapshot source, wrapped in tools.DescendantSnapshot so the batch's
//     membership is the CALLING scope's strict descendants (the root never);
//   - the stop delivery function, which must be the SAME Service whose rules
//     engine is routed into this batch — that pairing is what makes one arbiter
//     serve both halves (design §4.6). BatchConfig.Stop = svc.StopFunc() and the
//     binding's Arm (svc.SetArbiter(batch)) are therefore set together, here,
//     and nowhere else;
//   - the binding's Release, which puts the rules engine back on direct
//     delivery when the batch reports Done. A batch that is armed but no longer
//     ticked is worse than one that was never armed: rule firings queue into
//     it and drop past its queue cap.

import (
	"fmt"
	"os"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
)

// registerSuperviseTool registers agent mode's tick surface, and only then
// (design §4.7, §5 "default posture"): under policy none — the default — the
// tool is never registered, so an unconfigured session's tool surface is
// unchanged, and the belt is told the name so a persona that lists it is not
// reported as a typo.
//
// snap is the CONCRETE *supervision.Snapshotter on purpose. Taking it as
// supervisionapi.SnapshotSource would make the nil check below unfalsifiable:
// a nil *Snapshotter stored in an interface is a non-nil interface, so policy
// agent on a session whose run registry or trace store never came up would
// register a tool whose every tick reports an empty, done batch — the opposite
// of the honest refusal.
func registerSuperviseTool(st *loopInfraStage, cfg *config.Config, tr *toolRegistryStage, snap *supervision.Snapshotter) {
	if !cfg.Agent.Default.Supervisor.PolicyAgent() {
		// Under policy none — the default — the tool is never registered, so an
		// unconfigured session's tool surface is unchanged. Nothing is recorded
		// as omitted: the session is exactly what the operator asked for, and
		// the belt is told the name only so a persona listing it is not
		// reported as a typo.
		tr.belt.Know("supervise")
		return
	}
	if snap == nil || st.runs == nil || st.scopeMgr == nil || st.rootScopeID == "" {
		// policy: agent was configured and this session cannot serve it: the
		// trace store (snap), the run registry, the scope manager, or the root
		// scope never came up. The guard names all four because the batch binds
		// on all four (NewBatch(st.runs, &DescendantSnapshot{Source: snap, Scopes:
		// st.scopeMgr, Caller: callerScopeID}), NewSnapshotter(st.runs, st.scopeMgr,
		// snap, ...)): a nil scope manager would build a snapshot source that sees
		// no scope tree and answer every tick with the same silent empty-done
		// batch a nil snap would (review L2-r2), and an empty rootScopeID is the
		// caller the tool is registered with — begin() refuses every call without
		// it ("no calling scope is configured"), so the registered tool would be
		// one whose every tick fails (review r3 finding 3). That case is real: a
		// session whose CreateRootScope failed (ErrRootTerminal, or a degraded
		// buildGoatherdInfra) keeps its other three inputs and loses only the
		// root.
		// A supervise tool over such a source would answer every tick with an
		// empty, done batch and call that supervision, so it is not registered —
		// and the degradation is RECORDED rather than left silent: the name rides
		// the belt (an explicit tools list mentioning it is not a typo), it is
		// appended to omittedTools so the session's workspace record and the
		// isolated-mode prompt delta name it (registerWakeTools' pattern: "couldn't",
		// not "didn't"), and a startup warning says so once, because a policy the
		// operator asked for and did not get is a fact worth reading.
		tr.belt.Know("supervise")
		tr.omittedTools = append(tr.omittedTools, "supervise")
		fmt.Fprintf(os.Stderr, "warning: supervisor.policy is %q but this session has no supervision target (the run registry, the scope manager, the trace store, or the root scope did not start — a session whose root scope could not be created cannot supervise its own descendants), so the supervise tool is unavailable\n", cfg.Agent.Default.Supervisor.PolicyMode())
		return
	}
	// Admit widens the session's belt rather than trusting it, for the same
	// reason the MCP seam and the workflow pair do: the policy stanza is itself
	// the opt-in, and a curation pass must not silently drop the one tool the
	// operator asked for by configuring `policy: agent`.
	//
	// The consequence is stated because it is otherwise invisible: an EXPLICIT
	// agents.default.tools list is WIDENED by this one name — that list still
	// decides everything else the session holds, but "supervise" is admitted
	// whether or not it names it. Belt.Admit is the substrate's only widening
	// operation and it lives at the registration site for exactly this case; the
	// alternative (adding supervise to the curated defaultOrchestratorBelt) would
	// raise the per-call floor for EVERY orchestrator session, including the ones
	// that never enable a policy.
	tr.reg.Admit("supervise")
	tr.reg.Register(&tools.SuperviseTool{
		// The dispatching agent in agent mode is the session's own loop, so
		// the batch binds to the root scope's descendants. A nested
		// dispatching agent needs the runner to carry its scope on the tool
		// context (SuperviseTool.CallerScope is that seam); until then this
		// is the root agent, which is the mode's default caller.
		CallerScopeID: st.rootScopeID,
		NewBatch:      newSuperviseBatchFactory(st, cfg, snap),
	})
}

// superviseBatchConfig resolves the batch's tuning from the supervisor stanza
// (design §4.9). It is the ONE place a config key becomes a batch setting, for
// both modes: agent mode ticks the batch from the `supervise` tool, headless mode
// from the host loop, and both must run with the cadence, trigger, action mask,
// cooldown and cap the operator configured — otherwise the two modes the
// measurement compares would not be comparing the same loop.
//
// A nil cfg is the defaults: the accessors on SupervisorConfig supply the
// documented values (5s, one tick, unlimited, all three actions, always), so the
// zero-value stanza and an absent stanza behave identically.
func superviseBatchConfig(cfg *config.Config, callerScopeID string, stop supervision.StopFunc) supervision.BatchConfig {
	var sup config.SupervisorConfig
	if cfg != nil {
		sup = cfg.Agent.Default.Supervisor
	}
	return supervision.BatchConfig{
		// The batch's own root: the caller is never a member and never a
		// target. For the root agent that is exactly the session root the
		// design names; the descendant filter has already removed every
		// ancestor for a nested caller, so naming the caller here only
		// turns "not live" into the honest root_scope drop.
		RootScopeID:      callerScopeID,
		Interval:         sup.TickInterval(),
		CooldownTicks:    sup.CooldownTicks(),
		MaxInterventions: sup.MaxInterventions(),
		Actions:          policyActionMask(sup),
		Trigger:          policyTrigger(sup),
		Stop:             stop,
	}
}

// policyActionMask maps supervisor.actions onto the arbiter's vocabulary. An
// ABSENT key resolves to all three actions, while an explicit `actions: []`
// resolves to a non-nil EMPTY slice once mapped — which the arbiter reads as
// "apply nothing", the configuration's stated intent rather than a missing key.
func policyActionMask(sup config.SupervisorConfig) []supervisionapi.Action {
	names, _ := sup.ActionMask()
	out := make([]supervisionapi.Action, 0, len(names))
	for _, name := range names {
		switch name {
		case config.SupervisorActionSteer:
			out = append(out, supervisionapi.ActionSteer)
		case config.SupervisorActionRedirect:
			out = append(out, supervisionapi.ActionRedirect)
		case config.SupervisorActionRevert:
			out = append(out, supervisionapi.ActionRevert)
		}
	}
	return out
}

// policyTrigger maps supervisor.trigger onto the batch's trigger. Validation has
// already refused anything else, so the two-value switch is total.
func policyTrigger(sup config.SupervisorConfig) supervision.Trigger {
	if sup.TriggerMode() == config.SupervisorTriggerOnChange {
		return supervision.TriggerOnChange
	}
	return supervision.TriggerAlways
}

// newSuperviseBatchFactory builds the batch a `supervise` call binds on first
// use. The batch is created LATE — on the first call, not at startup — so a
// session that never supervises constructs nothing and leaves the rules
// engine's direct delivery exactly as it was.
func newSuperviseBatchFactory(st *loopInfraStage, cfg *config.Config, snap *supervision.Snapshotter) tools.SuperviseBatchFactory {
	return func(callerScopeID string) *tools.SuperviseBinding {
		batch := supervision.NewBatch(st.runs, &tools.DescendantSnapshot{
			Source: snap,
			Scopes: st.scopeMgr,
			Caller: callerScopeID,
		}, superviseBatchConfig(cfg, callerScopeID, superviseStopFunc(st.supervisorSvc)))

		binding := &tools.SuperviseBinding{Batch: batch}
		if svc := st.supervisorSvc; svc != nil {
			// One arbiter for rules and decisions (design §4.6): while the
			// batch ticks, the rules engine's firings queue for its next tick
			// instead of being delivered beside it. Release — called by the
			// tool when a tick reports Done — restores direct delivery, so a
			// loop the model has stopped driving cannot swallow the session's
			// rule firings. Both are Service mutex-guarded, so a Release and a
			// later Arm cannot interleave badly.
			binding.Arm = func() { svc.SetArbiter(batch) }
			binding.Release = func() { svc.SetArbiter(nil) }
		}
		return binding
	}
}

// superviseStopFunc adapts the Service's stop delivery. A nil service — tracing
// off, or a startup that never reached the goatherd branch — yields nil, which
// the arbiter records as no_stop_func rather than claiming a stop that never
// happened.
func superviseStopFunc(svc *supervision.Service) supervision.StopFunc {
	if svc == nil {
		return nil
	}
	return svc.StopFunc()
}
