// Tick types for the supervisor policy loop (yaah-k0x1, design §4.1 and
// §4.6): one Batch.Next call is one tick, and its TickResult carries the
// snapshot it read plus every decision's outcome. InterventionRecord is
// the record shape the dispatch envelope later cites (yaah-vaxv). Shapes
// only — Batch.Next itself is yaah-3l34.
package supervisionapi

import "time"

// Who decided an intervention: the tick's decider, an advisory rule, or
// the dispatching agent acting outside the tick loop through the
// supervisor tool.
const (
	// SourcePolicy marks an intervention the decider decided this tick.
	SourcePolicy = "policy"
	// SourceRules marks an intervention an advisory rule fired.
	SourceRules = "rules"
	// SourceRoot marks an intervention the dispatching agent itself took.
	SourceRoot = "root"
)

// What became of a decision this tick.
const (
	// OutcomeApplied marks a decision that took effect directly.
	OutcomeApplied = "applied"
	// OutcomeClaimed marks a decision whose effect a claim started and a
	// later adoption completes (a redirect or revert handoff).
	OutcomeClaimed = "claimed"
	// OutcomeDropped marks a decision that took no effect; DropReason
	// says why.
	OutcomeDropped = "dropped"
)

// DropReason values: why a dropped decision took no effect (yaah-3l34). The
// vocabulary is the arbiter's, in the order the arbiter asks the questions
// (design §4.6): target validation first, then precedence inside the tick,
// then the cooldown, the preconditions, the cap, and finally the claim.
//
// They are strings, not a typed enum, because they travel into the dispatch
// envelope (yaah-vaxv) and the supervise tool's reply (yaah-fy28), where a
// stable snake_case token is easier to read and to assert on than a number.
// A reason names the ONE question that failed, so a reason other than
// "stale"/"not_live" is evidence the decision was about a real live scope.
const (
	// DropReasonStale means the decision named a scope the batch has never
	// rendered and cannot resolve: a label from an older tick, or a typo.
	DropReasonStale = "stale"
	// DropReasonNotLive means the scope resolved but is not in the batch's
	// membership — it finished, or it was never a live non-root agent.
	DropReasonNotLive = "not_live"
	// DropReasonRootScope means the decision targeted the root scope, which
	// is never a target (the dispatching agent's own loop supervises).
	DropReasonRootScope = "root_scope"
	// DropReasonNotRedirectable means a redirect or revert named a scope
	// whose registration cannot be claimed for a handoff: the session root
	// (whose registration carries no handoff, and which is never
	// redirectable by design — §4.2), or any run that registered no
	// handoff at all. Redirectable is the ENFORCED boundary around the
	// root, asked as a target-validation question (before the cooldown and
	// the preconditions, because it is a fact about the target and not
	// about this tick) and asked AGAIN at claim time against the live
	// registration, so a claim is never made against an incarnation the
	// decider's snapshot described but the registry has since replaced.
	DropReasonNotRedirectable = "not_redirectable"
	// DropReasonUnknownAction means the action is not in the decision
	// vocabulary (none, steer, redirect, revert).
	DropReasonUnknownAction = "unknown_action"
	// DropReasonMaskedAction means the action is outside the configured
	// action mask (supervisor.actions).
	DropReasonMaskedAction = "masked_action"
	// DropReasonSuperseded means the rules half of this same tick outranked
	// the decision (design §4.6: stop > revert > redirect > steer). Two
	// cases only: a rules stop was DELIVERED on the scope this tick, or the
	// decision is a steer and a rules steer was attempted on the scope —
	// including one the cooldown dropped. A rules steer never suppresses a
	// policy revert or redirect, and a rule that accomplished nothing (no
	// stop delivery function, a refused or failed stop, empty guidance)
	// suppresses nothing.
	DropReasonSuperseded = "superseded"
	// DropReasonSupersededByStop means a rules stop voided a pending claim
	// made on an earlier tick: the claim will never reach adoption, because
	// a stop wins over a pending redirect.
	DropReasonSupersededByStop = "superseded_by_stop"
	// DropReasonDuplicate means the decision named a scope another decision
	// in the same list also named, at equal or higher precedence (one
	// decision per scope per tick).
	DropReasonDuplicate = "duplicate"
	// DropReasonCooldown means the scope was acted on within
	// cooldown_ticks ticks.
	DropReasonCooldown = "cooldown"
	// DropReasonEmptyGuidance means a steer, redirect or revert carried no
	// guidance to deliver.
	DropReasonEmptyGuidance = "empty_guidance"
	// DropReasonNotReversible means a revert targeted a scope with no pre-run
	// workspace capture. A revert is never downgraded to a redirect.
	DropReasonNotReversible = "not_reversible"
	// DropReasonCapReached means the batch's max_interventions budget is
	// spent.
	DropReasonCapReached = "cap_reached"
	// DropReasonAlreadyClaimed means another claimer won the same
	// registration generation first.
	DropReasonAlreadyClaimed = "already_claimed"
	// DropReasonNoRun means no run is registered under the scope (or the
	// registration generation is gone).
	DropReasonNoRun = "no_run"
	// DropReasonSteerBusy means the run's steering buffer is full: it is not
	// consuming guidance.
	DropReasonSteerBusy = "steer_busy"
	// DropReasonNoStopFunc means the batch had no stop delivery function, so
	// a rules stop was recorded but not delivered.
	DropReasonNoStopFunc = "no_stop_func"
	// DropReasonStopFailed means the injected stop delivery function
	// reported an error.
	DropReasonStopFailed = "stop_failed"
	// DropReasonStopRefused means the stop delivery side refused to act:
	// a root scope without supervisor.halt_root, a merged (terminal)
	// scope, or a run that registered no cancellation. Nothing was halted,
	// so nothing was claimed stopped and no pending claim was voided.
	DropReasonStopRefused = "stop_refused"
	// DropReasonNotDelivered means the DIRECT-delivery rules path (no batch
	// bound, so no tick arbitrated the action) could not hand the action to
	// anything: the scope was not addressable in the kernel, its own guidance
	// buffer refused the write, or the run bridge failed with an error it
	// could not classify. There is no tick to name and no claim behind it, so
	// the record says exactly that instead of leaving the firing silent.
	DropReasonNotDelivered = "not_delivered"
	// DropReasonQueueFull means the arbiter's rule queue was full when the
	// rules engine fired: the newest firing was discarded because the caller
	// stopped ticking (see maxQueuedRules). The drop happens on the rules
	// engine's goroutine, between ticks, which is why its record carries
	// Tick 0.
	DropReasonQueueFull = "queue_full"
)

// InterventionRecord is one decision with its outcome.
//
// It is the record the batch accumulates and the dispatch envelope carries
// beside the sub-agent's typed stop outcome (yaah-vaxv, design §4.10): every
// decision and every rules action gets one, executed or not, so a dispatching
// agent knows what happened to a run before it settles that run's proposal.
// An empty Guidance is "there was none to deliver".
type InterventionRecord struct {
	// At is the wall clock when the tick recorded the outcome.
	At time.Time
	// Tick is the batch tick that produced the decision. ZERO means the action
	// came from outside the tick loop: a rules action the Service delivered
	// directly (no batch arbitrated it), a firing the full rule queue
	// discarded, or the dispatching agent's own root action through the
	// supervisor tool. A reader orders by Tick and reads 0 as "between ticks".
	Tick int
	// Scope is the scope the decision named.
	Scope string
	// Source is SourcePolicy, SourceRules or SourceRoot.
	Source string
	// Action is the decided action.
	Action Action
	// Reason is the decider's reason.
	Reason string
	// Guidance is the decided guidance (empty for none).
	Guidance string
	// Outcome is OutcomeApplied, OutcomeClaimed or OutcomeDropped.
	Outcome string
	// DropReason is set only when Outcome is OutcomeDropped, for example
	// "not_reversible"; it names the ONE question that failed.
	DropReason string
	// Generation is the scope registration generation the intervention
	// targeted — the RedirectHandle.Gen type, so a claim and its record
	// name the same incarnation.
	Generation uint64
}

// TickResult is what one Batch.Next call returns. One call is one tick.
type TickResult struct {
	// Snapshot is the snapshot after this tick's ingest.
	Snapshot Snapshot
	// Applied is the decisions that took effect: outcome applied or
	// claimed. A subset of Results, in the same order.
	Applied []InterventionRecord
	// Results is every decision this tick with its outcome, in decision
	// order — including the dropped ones, which Applied omits.
	//
	// It also carries the tick's VOIDED CLAIMS: a claim a policy decision
	// made on an earlier tick, voided now because a rules stop was actually
	// delivered on the claimed generation. Such a record holds SourcePolicy
	// with the ORIGINAL action and generation, outcome dropped and
	// DropReasonSupersededByStop, and it directly follows the stop record
	// that voided it rather than sitting in decision order.
	Results []InterventionRecord
	// Done reports every scope in the batch has finished.
	Done bool
}
