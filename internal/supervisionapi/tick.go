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

// InterventionRecord is one decision with its outcome.
type InterventionRecord struct {
	// At is the wall clock when the tick recorded the outcome.
	At time.Time
	// Tick is the batch tick that produced the decision.
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
	// "not_reversible".
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
	Results []InterventionRecord
	// Done reports every scope in the batch has finished.
	Done bool
}
