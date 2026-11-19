// Proposal linearity is a dataflow analysis, not a second implementation of
// the executor. Every rule in the family answers one question this file
// settles for the whole graph: for each proposal a workflow can create, how
// many dispositions can a run apply to it, and can any run finish with it
// still open.
//
// The unit is a token: one proposal-producing node, keyed by that node's
// path. A proposal-lane dispatch yields a proposal every time it runs, so a
// fan-out body's delegate is one token produced once per item and a loop
// body's delegate is one token produced up to max_iterations times. The
// instance count is deliberately not modelled: a settlement step is written
// over the set (`steps.x.collect`) or over the single value
// (`steps.x.output`), so a settle that covers the token covers every
// instance of it, and collapsing instances is what keeps a repeated body
// checkable at all. The approximations that follow from the collapse are
// listed on Linearity.
//
// A token travels a control-flow relation rebuilt from the graph, and each
// part of it is read the way the scheduler reads it:
//
//   - needs orders two steps and joins on all of them — WF004's semantics,
//     reused because checking against an order the runtime will not perform
//     proves nothing;
//   - a reference is a data dependency. `proposals: {from: steps.build.collect}`
//     means the settle runs after the fan-out whether or not the author also
//     wrote the needs entry, so WF023 must not call those two steps parallel;
//   - branch arms and gate dispositions are alternative successors, joined
//     by union: that is what turns a proposal settled in one arm only into a
//     leak on the other;
//   - inside a body, nodes run in the order written, and a body's proposals
//     leave it only through its container's collect.
//
// The dataflow engine is split by concern: proposal.go holds the state
// lattice and the entry point, proposal_flow.go builds the flow relation and
// propagates tokens, and proposal_query.go answers the questions the
// WF020-WF029 rules in linearity.go ask.

package validate

import (
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// linearityState is the proof carried for one token across one node boundary.
//
// counts is a may-set of how many dispositions a run reaching the boundary
// has already applied: countNone for the runs that have not settled it,
// countOnce and countTwice for the runs that have. Keeping the three as bits
// rather than as a number is what makes this a may analysis over a finite
// lattice: a merge ORs the bits, the transfer shifts them, and a graph whose
// references happen to form a cycle still stops.
type linearityState struct {
	counts uint8
	// acts is the set of verbs applied along the runs counts describes.
	// WF022 reads it: a proposal some path already released must not be an
	// apply target further down.
	acts disposition
	// by names the nodes that applied a disposition on at least one path,
	// sorted and deduplicated so two runs over one graph agree byte for byte.
	by []string
}

// Disposition counts held by one token at one boundary.
const (
	countNone = 1 << iota
	countOnce
	countTwice
)

// disposition names a settlement verb, plus the one lifecycle verb the
// analysis must count: a continue_from supersedes its source, which disposes
// of it in exactly the sense WF021 means — the harness closes the parent when
// it starts the continuation.
type disposition uint8

// Disposition verbs.
const (
	dispApply disposition = 1 << iota
	dispSelect
	dispRelease
	dispDiscard
	dispSupersede
)

func (d disposition) String() string {
	var out []string
	for _, one := range []struct {
		bit  disposition
		name string
	}{
		{dispApply, "apply"},
		{dispSelect, "select"},
		{dispRelease, "release"},
		{dispDiscard, "discard"},
		{dispSupersede, "continue_from"},
	} {
		if d&one.bit != 0 {
			out = append(out, one.name)
		}
	}
	if len(out) == 0 {
		return "undisposed"
	}
	return strings.Join(out, "/")
}

// actionOf maps a settlement verb onto its disposition bit.
func actionOf(a ir.SettleAction) disposition {
	switch a {
	case ir.SettleApply:
		return dispApply
	case ir.SettleSelect:
		return dispSelect
	case ir.SettleRelease:
		return dispRelease
	case ir.SettleDiscard:
		return dispDiscard
	}
	return 0
}

// freshState is a token that exists and has no disposition yet.
func freshState() linearityState {
	return linearityState{counts: countNone}
}

// mergeState unions two boundary states: a run reaching the boundary came
// either way, so counts, verbs, and disposer names all join by union.
func mergeState(a, b linearityState) linearityState {
	out := linearityState{counts: a.counts | b.counts, acts: a.acts | b.acts}
	out.by = unionNames(a.by, b.by)
	return out
}

// namedBy returns the state after one step disposes of the token. The shift
// is the whole of the exactly-once bookkeeping: an undisposed token becomes
// disposed-once, and a token already disposed on some path becomes disposed
// twice on that path — which is the finding WF021 reports at the second
// disposer, because that is the step the author has to narrow or delete.
//
// Re-applying the same node's disposition is a no-op. Kleene rounds re-run a
// transfer on a grown entry, and without the guard a settle inside a loop
// body would shift its own token from once to twice on the second round and
// report a double settlement no run performs.
func namedBy(s linearityState, at string, act disposition) linearityState {
	if len(s.by) == 1 && s.by[0] == at {
		return s
	}
	out := linearityState{acts: s.acts | act, by: unionNames(s.by, []string{at})}
	if s.counts&countNone != 0 {
		out.counts |= countOnce
	}
	if s.counts&(countOnce|countTwice) != 0 {
		out.counts |= countTwice
	}
	if out.counts == 0 {
		// A state that arrives with no history at all: the token was named
		// by this step without an earlier production point on the path the
		// analysis walked, which is a first disposition, not a second one.
		out.counts = countOnce
	}
	return out
}

func unionNames(a, b []string) []string {
	if len(a) == 0 {
		return append([]string{}, b...)
	}
	if len(b) == 0 {
		return append([]string{}, a...)
	}
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// produces reports whether a node yields a proposal token. A proposal-lane
// dispatch is the obvious case, and a lane the author left unset counts too:
// resolve folds defaults, and an unset lane is the runtime's default lane —
// the same reading WF020 takes, and the one that keeps an unlabeled workstream
// a proposal. A select is the other producer: it hands on one candidate as the
// winner, and that winner is a proposal with a disposition still owed to it,
// under a fresh identity because the selection itself decides the candidates
// it rejected.
func produces(n *ir.Node) bool {
	switch n.Kind {
	case ir.KindDelegate:
		return n.Lane != ir.LaneReport
	case ir.KindSelect:
		return true
	}
	return false
}

// proposalSet is what a `settle.proposals` or `select.candidates` selector
// denotes, as far as one graph can say.
type proposalSet struct {
	// referents are the dispatch and selection nodes the handle names, in
	// document order.
	referents []*ir.Node
	// attributed is false for a handle the graph cannot trace to a producer —
	// a parameter or a fan-out item — which no linearity rule counts. An
	// attributed handle with no referents resolved to a step at all.
	attributed bool
	// known is false when the handle names no step it can attribute a lane
	// to: a param, an item, or a reference that resolves to nothing.
	known bool
	// singleton says the handle denotes at most one proposal, so two of its
	// elements cannot conflict.
	singleton bool
	// maybeEmpty says the handle can denote no proposals at all, which is
	// what makes `on_empty` a live decision.
	maybeEmpty bool
	// provablyEmpty says the document states the set has no elements at all:
	// an empty literal fan-out source, or an empty list default on a
	// parameter. This is stronger than maybeEmpty and is WF028's trigger.
	provablyEmpty bool
}

// producers is the subset of the referents that yield a proposal.
func (p proposalSet) producers() []*ir.Node {
	var out []*ir.Node
	for _, n := range p.referents {
		if produces(n) {
			out = append(out, n)
		}
	}
	return out
}

// reports is the subset that provably cannot carry a proposal: a dispatch the
// author labeled report lane. An unlabeled lane is the default lane and is not
// evidence of anything, which is why this is narrower than "not a producer".
func (p proposalSet) reports() []*ir.Node {
	var out []*ir.Node
	for _, n := range p.referents {
		if n.Kind == ir.KindDelegate && n.Lane == ir.LaneReport {
			out = append(out, n)
		}
	}
	return out
}

// ids are the token paths the handle denotes.
func (p proposalSet) ids() []string {
	var out []string
	for _, n := range p.producers() {
		out = append(out, n.Path)
	}
	return out
}

// String renders the handle's referents for a message, bounded so a wide
// collect cannot bury the finding.
func (p proposalSet) String() string {
	if len(p.referents) == 0 {
		return "no dispatch this graph can attribute"
	}
	names := make([]string, 0, len(p.referents))
	for _, n := range p.referents {
		names = append(names, n.Path)
	}
	if len(names) > 3 {
		names = append(names[:3], "…")
	}
	return strings.Join(names, ", ")
}

// linearity is one analysis of one graph: the flow relation, the boundary
// states, and the residue the exit rules read. Nothing here survives the call
// — a validator holds no state between graphs.
type linearity struct {
	g      *ir.Graph
	order  []*ir.Node
	parent map[*ir.Node]*ir.Node
	top    map[string]*ir.Node
	// scope is a node's visible step ids: every top-level step, plus the
	// body-local ids of every container it sits in up to and including the
	// node itself, which is the namespace ir.resolveBody compiled its
	// references against.
	scope map[*ir.Node]map[string]*ir.Node

	// preds and succs hold external edges only — needs, data reads, arm
	// targets. Container-to-body and sibling order are structural and are
	// walked by compute, so a body node never appears in another
	// container's predecessor list.
	preds map[*ir.Node][]*ir.Node
	succs map[*ir.Node][]*ir.Node
	// runEnds are the boundaries a run can finish at.
	runEnds []*ir.Node

	entry map[*ir.Node]map[string]linearityState
	exit  map[*ir.Node]map[string]linearityState
	// runExit merges every run end: the exit state WF021 and WF025 read.
	runExit map[string]linearityState
	// seen accumulates every token the analysis met, for the proof artifact.
	seen map[string]linearityState
	// stranded records a proposal produced in a body whose container declares
	// no collect: no step outside can ever name it, so no settle can dispose
	// of it and the run cannot end any other way.
	stranded map[*ir.Node]bool

	// settledBy indexes the settle and select nodes that name each token and
	// consumedBy the continue_from nodes that supersede it, both in document
	// order — the shape the pairwise rules query without re-walking
	// selectors.
	settledBy  map[string][]*ir.Node
	consumedBy map[string][]*ir.Node
	// claimed marks the tokens whose existence as proposals the document
	// itself states, which is the set the leak rules may speak about.
	claimed map[string]bool
	// guarded marks the nodes that sit behind a branch arm or a gate
	// disposition, whose state therefore describes an alternative path rather
	// than a step the scheduler waited for.
	guarded map[*ir.Node]bool
}

// analyze builds the flow relation and runs the dataflow to a fixed point.
func analyze(g *ir.Graph) *linearity {
	l := &linearity{
		g:          g,
		top:        stepsByID(g),
		scope:      map[*ir.Node]map[string]*ir.Node{},
		preds:      map[*ir.Node][]*ir.Node{},
		succs:      map[*ir.Node][]*ir.Node{},
		entry:      map[*ir.Node]map[string]linearityState{},
		exit:       map[*ir.Node]map[string]linearityState{},
		seen:       map[string]linearityState{},
		stranded:   map[*ir.Node]bool{},
		settledBy:  map[string][]*ir.Node{},
		consumedBy: map[string][]*ir.Node{},
		claimed:    map[string]bool{},
		guarded:    map[*ir.Node]bool{},
	}
	l.order, l.parent = graphNodes(g)
	l.build()
	l.propagate()
	l.indexDispositions()
	l.claimScope()
	return l
}
