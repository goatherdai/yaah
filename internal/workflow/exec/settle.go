// Settlement (yaah-0u0v.13). A settle node is an ADAPTER over the proposal
// runtime, not a second settlement implementation: the scheduler resolves
// which proposals it owns from recorded run state, asks the door to dispose of
// them one at a time, and records what came back. Nothing here merges a tree,
// names a worktree, or knows what an apply strategy is — those are
// internal/dispatch's, reached through Runtime.Settle by the host door.
//
// The split follows the package's one rule: the driver calls the door and the
// reducer owns the state. settle.go holds the vocabulary and scheduler flow,
// settle_handles.go resolves proposal handles from run state, and
// settle_values.go projects and filters the values those handles name.

package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// Reasons and statuses the settlement family states.
const (
	// ReasonProposalAlreadySettled is the runtime linearity guard: a proposal
	// has exactly one disposition, so a settle naming an id this run already
	// disposed of is refused before any settlement is asked for.
	ReasonProposalAlreadySettled = "proposal_already_settled"
	// ReasonProposalSettleInFlight is the same guard seen from the other side:
	// another settle node holds the proposal right now.
	ReasonProposalSettleInFlight = "proposal_settle_in_flight"
	// ReasonOpenProposalsAtExit is the run-level half of the guard: a run must
	// not report success while a proposal it sealed has no disposition. The
	// v1 vocabulary declares no on_abort field for that residue (WF025 says so
	// at its own rule), so the escape is the caller's Config.OpenProposals.
	ReasonOpenProposalsAtExit = "open_proposals_at_run_end"
	// ReasonSettlementFailed prefixes a door malfunction or a settlement whose
	// status came back neither settled nor conflict.
	ReasonSettlementFailed = "settlement_failed"
	// ReasonSettleActionUnsupported names a settle action outside the four
	// settlement verbs. continue and fork are NOT settlement verbs — they are
	// re-run bindings on a delegate (continue_from / fork_from) — so they are
	// refused here rather than mapped.
	ReasonSettleActionUnsupported = "unsupported_settle_action"
	// ReasonSettleSourceUnavailable prefixes a proposals or where handle the
	// scheduler cannot resolve from recorded run state: an output carrying no
	// proposal handle, an item binding inside a container body, or a filter
	// field no handle has.
	ReasonSettleSourceUnavailable = "settle_source_unavailable"
	// ReasonNoProposalsMatched is on_empty: fail — matching nothing was an
	// outcome the author declared unacceptable.
	ReasonNoProposalsMatched = "no_proposals_matched"
)

// Settlement statuses, verbatim from the proposal runtime's one settlement
// envelope. The scheduler never synthesizes one.

// Settlement statuses, verbatim from the proposal runtime's one settlement
// envelope. The scheduler never synthesizes one.
const (
	SettlementStatusSettled  = "settled"
	SettlementStatusConflict = "conflict"
	SettlementStatusError    = "error"
)

// ProposalHandle is the typed record of a proposal an attempt sealed. The
// envelope's proposal_id is the whole fact; the rest is what a settle node and
// a later trace need in order to name where it came from without re-deriving
// it from per-lane plumbing.

// ProposalHandle is the typed record of a proposal an attempt sealed. The
// envelope's proposal_id is the whole fact; the rest is what a settle node and
// a later trace need in order to name where it came from without re-deriving
// it from per-lane plumbing.
type ProposalHandle struct {
	ProposalID string `json:"proposal_id"`
	// Producer is the node path that sealed it.
	Producer   string `json:"producer,omitempty"`
	Role       string `json:"role,omitempty"`
	Lane       string `json:"lane,omitempty"`
	DispatchID string `json:"dispatch_id,omitempty"`
}

// ProposalRef is one element of a settle node's resolved handle set.

// ProposalRef is one element of a settle node's resolved handle set.
type ProposalRef struct {
	ProposalID string `json:"proposal_id"`
	Producer   string `json:"producer,omitempty"`
	// Fields are the remaining bindings the source element carried beside
	// the id. A settle whose where reads the SAME source as its proposals
	// — the gate loop's collected check reports, each naming the fix
	// proposal it verified and the verdict it reached — filters on them
	// without any join across two steps' sets (yaah-0u0v.27). The three
	// fields above stay first-class: they are the handle's identity, not
	// element data.
	Fields map[string]any `json:"fields,omitempty"`
}

// SettlePlan is one settle node's resolved work: the handles to dispose of, in
// the order the door is asked to take them, plus the dispositions the author
// declared for what the door reports back.

// SettlePlan is one settle node's resolved work: the handles to dispose of, in
// the order the door is asked to take them, plus the dispositions the author
// declared for what the door reports back.
type SettlePlan struct {
	Refs       []ProposalRef     `json:"refs"`
	Action     ir.SettleAction   `json:"action"`
	Policy     ir.SettlePolicy   `json:"policy,omitempty"`
	Order      ir.SettleOrder    `json:"order,omitempty"`
	OnConflict ir.ConflictPolicy `json:"on_conflict,omitempty"`
	Empty      ir.EmptyPolicy    `json:"on_empty,omitempty"`
}

// Settled is the scheduler's view of one settlement. It mirrors the proposal
// runtime's settlement envelope the way Completion mirrors the dispatch
// envelope — same facts, separate type, because this package may not import
// internal/dispatch. internal/workflow/host is the producer that bridges them.

// Settled is the scheduler's view of one settlement. It mirrors the proposal
// runtime's settlement envelope the way Completion mirrors the dispatch
// envelope — same facts, separate type, because this package may not import
// internal/dispatch. internal/workflow/host is the producer that bridges them.
type Settled struct {
	ProposalID string `json:"proposal_id"`
	Producer   string `json:"producer,omitempty"`
	// Action is the verb the door was asked to apply.
	Action string `json:"action,omitempty"`
	// Status is settled, conflict, or error — verbatim.
	Status string `json:"status,omitempty"`
	// Outcome is the runtime's typed work/scope/record/notes block. It is
	// multi-line prose and therefore never reaches an event log line.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// Conflicts are the overlap paths a refused apply named.
	Conflicts []string `json:"conflicts,omitempty"`
	// Files are the paths the settlement touched, when the envelope reported.
	Files []string `json:"files,omitempty"`
	// LeftOpen reports that the proposal has no disposition now: a conflict
	// leaves the worktree standing, and an error leaves everything standing.
	LeftOpen bool `json:"left_open,omitempty"`
}

// settlementVerb maps a resolved settle action onto the door's verb. The
// vocabulary is exactly four and the default arm refuses everything else —
// which is where continue and fork are turned away.

// settlementVerb maps a resolved settle action onto the door's verb. The
// vocabulary is exactly four and the default arm refuses everything else —
// which is where continue and fork are turned away.
func settlementVerb(a ir.SettleAction) (string, bool) {
	switch a {
	case ir.SettleSelect:
		return "select", true
	case ir.SettleApply:
		return "apply", true
	case ir.SettleRelease:
		return "release", true
	case ir.SettleDiscard:
		return "discard", true
	}
	return "", false
}

// nextSettlePath returns the first ready settle node the driver may work on.
// The ready queue is kept in canonical path order, so a graph with two settle
// nodes plans and settles them in the same order every run.

// nextSettlePath returns the first ready settle node the driver may work on.
// The ready queue is kept in canonical path order, so a graph with two settle
// nodes plans and settles them in the same order every run.
func (r *Run) nextSettlePath() (string, bool) {
	for _, p := range r.ready {
		n := r.byPath[p]
		if n == nil || n.Kind != ir.KindSettle {
			continue
		}
		st := r.state[p]
		if st.State != StateReady {
			continue
		}
		if st.SettlePlan == nil || st.Settling < len(st.SettlePlan.Refs) {
			return p, true
		}
	}
	return "", false
}

// settleStep performs one settlement — or one plan report — for one ready
// settle node, and submits the resulting event. It mutates nothing itself:
// both the plan and each disposition are decided inside Apply.

// settleStep performs one settlement — or one plan report — for one ready
// settle node, and submits the resulting event. It mutates nothing itself:
// both the plan and each disposition are decided inside Apply.
func (r *Run) settleStep(ctx context.Context, path string) (bool, error) {
	n := r.byPath[path]
	st := r.state[path]
	if n == nil || st == nil {
		return false, nil
	}
	if st.SettlePlan == nil {
		refs, deferred, refusal := r.resolveHandles(n)
		if deferred {
			return false, nil
		}
		return true, r.Apply(SettlementPlannedEvent{
			NodePath:   path,
			Refs:       refs,
			Action:     n.Action,
			Policy:     n.SettlePolicy,
			Order:      n.Order,
			OnConflict: n.OnConflict,
			Empty:      n.OnEmpty,
			Refusal:    refusal,
		})
	}
	if st.Settling >= len(st.SettlePlan.Refs) {
		return false, nil
	}
	ref := st.SettlePlan.Refs[st.Settling]
	verb, ok := settlementVerb(st.SettlePlan.Action)
	if !ok {
		return true, r.Apply(ProposalSettledEvent{
			NodePath: path,
			Settled:  Settled{ProposalID: ref.ProposalID, Producer: ref.Producer, Status: SettlementStatusError, Action: string(st.SettlePlan.Action), LeftOpen: true},
			// An unmappable action is a door-side refusal, not a
			// settlement the lane reported: no proposal was touched.
			Refused: ReasonSettleActionUnsupported + ": " + string(st.SettlePlan.Action) +
				"; the settlement vocabulary is select|apply|release|discard",
		})
	}
	// The intent is recorded — and, with a Store configured, persisted — before
	// the door is asked, so a crash between the two leaves "asked, outcome
	// unknown" on the record instead of a silent gap that a resume would fill by
	// asking a second time (yaah-0u0v.16).
	if err := r.Apply(SettlementAskedEvent{NodePath: path, ProposalID: ref.ProposalID, Action: verb}); err != nil {
		return true, err
	}
	s, err := r.rt.Settle(ctx, SettleRequest{
		NodePath:   path,
		ProposalID: ref.ProposalID,
		Producer:   ref.Producer,
		Verb:       verb,
		Policy:     st.SettlePlan.Policy,
	})
	if s.ProposalID == "" {
		s.ProposalID = ref.ProposalID
	}
	if s.Producer == "" {
		s.Producer = ref.Producer
	}
	s.Action = verb
	ev := ProposalSettledEvent{NodePath: path, Settled: s}
	if err != nil {
		// A door malfunction is reported as an error disposition: there is no
		// envelope behind it, so the scheduler asserts nothing about what the
		// world did and leaves the proposal open.
		ev.DoorError = err.Error()
		if s.Status == "" {
			s.Status = SettlementStatusError
		}
		ev.Settled = s
	}
	return true, r.Apply(ev)
}

// onSettlementPlanned records a settle node's resolved handle set and applies
// the two rules that decide whether the node may work at all: on_empty on an
// empty set, and the linearity guard on a proposal this run already owns.
//
// The guard is checked before the first settlement is asked for, so a node
// that names one already-disposed proposal settles nothing — not even the
// other elements of its own set.

// onSettlementPlanned records a settle node's resolved handle set and applies
// the two rules that decide whether the node may work at all: on_empty on an
// empty set, and the linearity guard on a proposal this run already owns.
//
// The guard is checked before the first settlement is asked for, so a node
// that names one already-disposed proposal settles nothing — not even the
// other elements of its own set.
func (r *Run) onSettlementPlanned(e SettlementPlannedEvent) error {
	st := r.state[e.NodePath]
	if st == nil {
		return fmt.Errorf("workflow/exec: settlement_planned for unknown node %q", e.NodePath)
	}
	if st.State != StateReady {
		return fmt.Errorf("workflow/exec: settlement_planned for node %s in state %s", e.NodePath, st.State)
	}
	n := r.byPath[e.NodePath]
	if e.Refusal != "" {
		return r.failTerminal(e.NodePath, e.Refusal)
	}
	if _, ok := settlementVerb(e.Action); !ok {
		return r.failTerminal(e.NodePath, ReasonSettleActionUnsupported+": "+string(e.Action)+
			"; the settlement vocabulary is select|apply|release|discard")
	}
	if len(e.Refs) == 0 {
		st.SettlePlan = &SettlePlan{Action: e.Action, Policy: e.Policy, Order: e.Order, OnConflict: e.OnConflict, Empty: e.Empty}
		switch e.Empty {
		case ir.OnEmptyFail:
			return r.failTerminal(e.NodePath, ReasonNoProposalsMatched+": "+e.NodePath+
				" resolved no proposals and declares on_empty: fail")
		case ir.OnEmptySkip:
			return r.derive(NodeSkippedEvent{
				NodePath: e.NodePath,
				Reason:   ReasonNoProposalsMatched + ": " + e.NodePath + " resolved no proposals (on_empty: skip)",
			})
		default:
			// pass is the declared answer and the folded default: nothing to
			// dispose of is a completed settle, and the run says so through
			// the node's recorded value rather than through a warning.
			return r.closeSettlement(n, st)
		}
	}
	for _, ref := range e.Refs {
		if by, done := r.settled[ref.ProposalID]; done {
			return r.failTerminal(e.NodePath, ReasonProposalAlreadySettled+": proposal "+ref.ProposalID+
				" is already disposed of by "+by+"; a proposal has exactly one disposition")
		}
		if by, held := r.claims[ref.ProposalID]; held && by != e.NodePath {
			return r.failTerminal(e.NodePath, ReasonProposalSettleInFlight+": proposal "+ref.ProposalID+
				" is being settled by "+by)
		}
	}
	plan := &SettlePlan{
		Refs:       append([]ProposalRef(nil), e.Refs...),
		Action:     e.Action,
		Policy:     e.Policy,
		Order:      e.Order,
		OnConflict: e.OnConflict,
		Empty:      e.Empty,
	}
	st.SettlePlan = plan
	st.Settling = 0
	st.Settled = nil
	for _, ref := range plan.Refs {
		r.claims[ref.ProposalID] = e.NodePath
	}
	return nil
}

// onSettlementAsked records the disposition the run is about to ask the door
// for. It is the settlement write-ahead record (yaah-0u0v.16): the driver emits
// this BEFORE Runtime.Settle, so the checkpoint says "asked" before the world
// effect can happen, and a resumed run can tell an unknown outcome from a
// settlement that never started.

// onSettlementAsked records the disposition the run is about to ask the door
// for. It is the settlement write-ahead record (yaah-0u0v.16): the driver emits
// this BEFORE Runtime.Settle, so the checkpoint says "asked" before the world
// effect can happen, and a resumed run can tell an unknown outcome from a
// settlement that never started.
func (r *Run) onSettlementAsked(e SettlementAskedEvent) error {
	st := r.state[e.NodePath]
	if st == nil {
		return fmt.Errorf("workflow/exec: settlement_asked for unknown node %q", e.NodePath)
	}
	if st.SettlePlan == nil {
		// A join:any|quorum container disposes of the proposals its NON-winning
		// items sealed, through this same door and the discard verb
		// (yaah-0u0v.19). The check precedes the state guard because that
		// container is TERMINAL by design: the join was decided before the
		// residue was known. It keeps the same write-ahead record a settle node
		// does, so a resume can tell "asked, outcome unknown" from "never asked".
		if n := r.byPath[e.NodePath]; n != nil && n.Kind == ir.KindFanout && cancelJoin(n) {
			return r.onJoinDiscardAsked(e.NodePath, st, e)
		}
		return fmt.Errorf("workflow/exec: settlement_asked for node %s with no planned settlement", e.NodePath)
	}
	if st.State != StateReady {
		return fmt.Errorf("workflow/exec: settlement_asked for node %s in state %s", e.NodePath, st.State)
	}
	if st.Asked != "" {
		return fmt.Errorf("workflow/exec: settlement_asked for node %s while proposal %s is still unanswered",
			e.NodePath, st.Asked)
	}
	if e.ProposalID == "" {
		return fmt.Errorf("workflow/exec: settlement_asked for node %s with no proposal id", e.NodePath)
	}
	st.Asked = e.ProposalID
	return nil
}

// onProposalSettled folds one disposition into the node's record and decides
// what it means for the node: an error ends it, a conflict ends it unless the
// author declared skip, and the last element of the plan closes it succeeded.

// onProposalSettled folds one disposition into the node's record and decides
// what it means for the node: an error ends it, a conflict ends it unless the
// author declared skip, and the last element of the plan closes it succeeded.
func (r *Run) onProposalSettled(e ProposalSettledEvent) error {
	st := r.state[e.NodePath]
	if st == nil {
		return fmt.Errorf("workflow/exec: proposal_settled for unknown node %q", e.NodePath)
	}
	if st.SettlePlan == nil {
		if n := r.byPath[e.NodePath]; n != nil && n.Kind == ir.KindFanout && cancelJoin(n) {
			return r.onJoinDiscardSettled(e.NodePath, st, e)
		}
		return fmt.Errorf("workflow/exec: proposal_settled for node %s with no planned settlement", e.NodePath)
	}
	if st.State != StateReady {
		return fmt.Errorf("workflow/exec: proposal_settled for node %s in state %s", e.NodePath, st.State)
	}
	// The unanswered ask is discharged by any answer, including a refusal: what
	// Asked records is "a disposition is in flight", and after this event none
	// is.
	st.Asked = ""
	if e.DoorError != "" {
		st.SettlePlan = nil
		return r.failTerminal(e.NodePath, ReasonSettlementFailed+": "+e.NodePath+": "+e.DoorError)
	}
	s := e.Settled
	if e.Refused != "" {
		st.SettlePlan = nil
		return r.failTerminal(e.NodePath, e.Refused)
	}
	st.Settled = append(st.Settled, s)
	st.Settling++
	// Disposition bookkeeping. A settled proposal leaves the open set and
	// takes a permanent claim; a conflict or an error leaves it open and
	// releases the claim, which is what lets the downstream catch settle
	// WF024 demands for on_conflict: skip.
	delete(r.claims, s.ProposalID)
	switch {
	case s.Status == SettlementStatusSettled && !s.LeftOpen:
		r.settled[s.ProposalID] = e.NodePath
		delete(r.open, s.ProposalID)
	default:
		if _, seen := r.open[s.ProposalID]; !seen {
			r.open[s.ProposalID] = s.Producer
		}
	}
	n := r.byPath[e.NodePath]
	switch {
	case s.Status == SettlementStatusError:
		return r.failTerminal(e.NodePath, ReasonSettlementFailed+": proposal "+s.ProposalID+
			" action "+s.Action+" status=error error="+flatten(s.Error))
	case s.Status == SettlementStatusConflict && n != nil && n.OnConflict != ir.OnConflictSkip:
		reason := ReasonSettlementFailed + ": conflict on proposal " + s.ProposalID +
			" action " + s.Action + " paths=" + orDash(strings.Join(s.Conflicts, ","))
		if n.OnConflict == ir.OnConflictEscalate {
			// The escalation target is a human response, and a human response
			// is an approval door the scheduler core does not have. The
			// conflict is surfaced as the node's failure reason, naming what
			// is missing rather than pretending it was answered.
			reason += " (on_conflict: escalate needs a human response door, which is not the scheduler core)"
		}
		return r.failTerminal(e.NodePath, reason)
	}
	if st.Settling < len(st.SettlePlan.Refs) {
		return nil
	}
	return r.closeSettlement(n, st)
}

// closeSettlement records the node's settled value and closes it. The value is
// the structured record a later emit reads through `steps.<id>.output.settled`,
// and the same record the run's output list carries, so the two can never
// disagree.

// closeSettlement records the node's settled value and closes it. The value is
// the structured record a later emit reads through `steps.<id>.output.settled`,
// and the same record the run's output list carries, so the two can never
// disagree.
func (r *Run) closeSettlement(n *ir.Node, st *NodeState) error {
	encoded, err := json.Marshal(st.settledValue())
	if err != nil {
		return r.failTerminal(st.Path, ReasonSettlementFailed+": encoding the settled value: "+err.Error())
	}
	st.Result = string(encoded)
	if n != nil {
		r.recordOutput(st, NodeOutput{
			NodePath: n.Path,
			Kind:     n.Kind,
			Name:     settleBindingName(n),
			Value:    st.Result,
			Contract: st.Contract,
		})
	}
	return r.derive(NodeSucceededEvent{NodePath: st.Path, Outputs: outputNames(st)})
}

// settleBindingName is the name a settle's value is bound under: its declared
// output name, else its step id, else its path — the same rule emit follows,
// so `steps.<id>.output` and the named binding read one value.

// settleBindingName is the name a settle's value is bound under: its declared
// output name, else its step id, else its path — the same rule emit follows,
// so `steps.<id>.output` and the named binding read one value.
func settleBindingName(n *ir.Node) string {
	if n.Output != nil && n.Output.Name != "" {
		return n.Output.Name
	}
	if n.ID != "" {
		return n.ID
	}
	return n.Path
}

// settledValue is the structured value a settled node reports: the counts an
// author interpolates into a report, plus the per-proposal record a trace
// reads. Maps are built from ordered slices and encoded by encoding/json, so
// the text is a function of the dispositions and nothing else.

// settledValue is the structured value a settled node reports: the counts an
// author interpolates into a report, plus the per-proposal record a trace
// reads. Maps are built from ordered slices and encoded by encoding/json, so
// the text is a function of the dispositions and nothing else.
func (st *NodeState) settledValue() map[string]any {
	var settled, conflicted, leftOpen int
	ids := make([]string, 0, len(st.Settled))
	results := make([]any, 0, len(st.Settled))
	for _, s := range st.Settled {
		m := map[string]any{
			"proposal_id": s.ProposalID,
			"action":      s.Action,
			"status":      s.Status,
		}
		if s.Producer != "" {
			m["producer"] = s.Producer
		}
		if len(s.Conflicts) > 0 {
			m["conflicts"] = append([]string(nil), s.Conflicts...)
			conflicted++
		}
		if len(s.Files) > 0 {
			m["files"] = append([]string(nil), s.Files...)
		}
		if s.Error != "" {
			m["error"] = s.Error
		}
		if s.Outcome != "" {
			m["outcome"] = s.Outcome
		}
		switch {
		case s.LeftOpen || s.Status != SettlementStatusSettled:
			leftOpen++
		default:
			settled++
			ids = append(ids, s.ProposalID)
		}
		results = append(results, m)
	}
	return map[string]any{
		"settled":    settled,
		"conflicted": conflicted,
		"left_open":  leftOpen,
		"ids":        ids,
		"results":    results,
	}
}

// openProposals lists the proposal ids this run holds without a disposition,
// sorted, so the reason a run refuses to end succeeded names the same list
// every time.

// openProposals lists the proposal ids this run holds without a disposition,
// sorted, so the reason a run refuses to end succeeded names the same list
// every time.
func (r *Run) openProposals() []string {
	out := make([]string, 0, len(r.open))
	for id := range r.open {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// openProposalDetail renders the residue for the run's reason line.

// openProposalDetail renders the residue for the run's reason line.
func (r *Run) openProposalDetail() string {
	ids := r.openProposals()
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if by := r.open[id]; by != "" {
			parts = append(parts, id+" ("+by+")")
			continue
		}
		parts = append(parts, id)
	}
	return strings.Join(parts, ", ")
}

// releaseClaims drops the claims a node holds on proposals it will not settle
// after all. It runs when a settle node terminates, so a node that failed
// halfway through its plan leaves the rest of its set settle-able instead of
// walled off, and it never touches a proposal already disposed of.

// releaseClaims drops the claims a node holds on proposals it will not settle
// after all. It runs when a settle node terminates, so a node that failed
// halfway through its plan leaves the rest of its set settle-able instead of
// walled off, and it never touches a proposal already disposed of.
func (r *Run) releaseClaims(path string) {
	st := r.state[path]
	if st == nil || st.SettlePlan == nil {
		return
	}
	for i := st.Settling; i < len(st.SettlePlan.Refs); i++ {
		id := st.SettlePlan.Refs[i].ProposalID
		if _, done := r.settled[id]; done {
			continue
		}
		if holder, ok := r.claims[id]; ok && holder == path {
			delete(r.claims, id)
		}
	}
}

// resolveHandles turns a settle node's proposals and where selectors into the
// refs it disposes of, and answers three things at once: whether the node may
// be planned yet (a producer that has not terminated is a wait, not an empty
// set), what the set is, and whether the declaration is something this bead
// can execute at all.
