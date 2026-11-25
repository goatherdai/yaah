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
	if snap == nil || st.runs == nil || st.scopeMgr == nil {
		// policy: agent was configured and this session cannot serve it: the
		// trace store (snap), the run registry, or the scope manager never came
		// up. The guard names all three because the batch binds on all three
		// (NewBatch(st.runs, &DescendantSnapshot{Source: snap, Scopes:
		// st.scopeMgr, ...}), NewSnapshotter(st.runs, st.scopeMgr, snap, ...)):
		// a nil scope manager would build a snapshot source that sees no scope
		// tree and answer every tick with the same silent empty-done batch a nil
		// snap would (review L2-r2).
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
		fmt.Fprintf(os.Stderr, "warning: supervisor.policy is %q but this session has no snapshot source (the run registry, the scope manager, or the trace store did not start), so the supervise tool is unavailable\n", cfg.Agent.Default.Supervisor.PolicyMode())
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
		NewBatch:      newSuperviseBatchFactory(st, snap),
	})
}

// superviseBatchConfig is the BatchConfig the production factory builds. It is
// a function so the settings exist in exactly one place and are pinned by a
// test (supervise_registration_test.go): CooldownTicks is the design's default
// selector — a NEGATIVE value means supervision.DefaultCooldownTicks, i.e. the
// `cooldown_ticks: 1` the tool's doc and design §4.9 promise. The ZERO value
// means "no cooldown", which is why leaving the field unset here would silently
// disable a documented brake.
func superviseBatchConfig(callerScopeID string, stop supervision.StopFunc) supervision.BatchConfig {
	return supervision.BatchConfig{
		// The batch's own root: the caller is never a member and never a
		// target. For the root agent that is exactly the session root the
		// design names; the descendant filter has already removed every
		// ancestor for a nested caller, so naming the caller here only
		// turns "not live" into the honest root_scope drop.
		RootScopeID: callerScopeID,
		// -1 selects the documented default (one tick), never 0.
		CooldownTicks: -1,
		Stop:          stop,
	}
}

// newSuperviseBatchFactory builds the batch a `supervise` call binds on first
// use. The batch is created LATE — on the first call, not at startup — so a
// session that never supervises constructs nothing and leaves the rules
// engine's direct delivery exactly as it was.
func newSuperviseBatchFactory(st *loopInfraStage, snap *supervision.Snapshotter) tools.SuperviseBatchFactory {
	return func(callerScopeID string) *tools.SuperviseBinding {
		batch := supervision.NewBatch(st.runs, &tools.DescendantSnapshot{
			Source: snap,
			Scopes: st.scopeMgr,
			Caller: callerScopeID,
		}, superviseBatchConfig(callerScopeID, superviseStopFunc(st.supervisorSvc)))

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
