// Proposal linearity: queries read by the WF020-WF029 rule functions.

package validate

import (
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// consumed is the token a continue_from supersedes. Only continue_from
// consumes: fork_from leaves its source settle-able, which is WF027's whole
// point, and seed_from threads a conversation without touching a proposal's
// disposition at all. A template that names nothing, or more than one
// proposal, is WF027's finding and is not counted here.
func (l *linearity) consumed(n *ir.Node) string {
	if n.ContinueFrom == nil {
		return ""
	}
	if ps := l.handleFor(n, selectorOfSource(templateSource(n.ContinueFrom))).producers(); len(ps) == 1 {
		return ps[0].Path
	}
	return ""
}

// indexDispositions records which steps name each token.
func (l *linearity) indexDispositions() {
	for _, n := range l.order {
		switch n.Kind {
		case ir.KindSettle:
			for _, id := range l.handleFor(n, n.Proposals).ids() {
				l.settledBy[id] = append(l.settledBy[id], n)
			}
		case ir.KindSelect:
			for _, id := range l.handleFor(n, n.Candidates).ids() {
				l.settledBy[id] = append(l.settledBy[id], n)
			}
		case ir.KindLoop, ir.KindDelegate:
			if id := l.consumed(n); id != "" {
				l.consumedBy[id] = append(l.consumedBy[id], n)
			}
		}
	}
}

// claimScope decides which tokens the leak rules may call a proposal. The
// linearity family's subject is a document's settlement plan, so a token is
// in plan when the author put it there one of four ways: a handle names it
// (a settle, a selection, or a continuation already reaches for it, which is
// the strongest statement available), the dispatch declares the proposal lane
// outright, the dispatch types its output as a Proposal, or the token is the
// winner of a select whose candidates are themselves in plan.
//
// The gate is what keeps this family honest about the graphs it is handed.
// A delegate whose lane nobody wrote down is a dispatch on the runtime default
// lane, and the default lane is not a retention claim this package can read as
// one — WF011 is the rule that refuses a producer whose *declared* type
// contradicts a settle. So a workflow that never mentions settlement, which is
// most of the corpus and every fixture written for another family's rule, is
// not a workflow with a leak: it is a workflow with no settlement plan, and
// there is nothing here to prove. Without this gate WF021 reports a leak for
// every dispatch in the graph, which is a finding no author can act on and a
// rule that cannot be wired into Validate at all.
func (l *linearity) claimScope() {
	for _, n := range l.order {
		if !produces(n) {
			continue
		}
		switch {
		case len(l.settledBy[n.Path]) > 0 || len(l.consumedBy[n.Path]) > 0:
			l.claimed[n.Path] = true
		case n.Kind == ir.KindDelegate && n.Lane == ir.LaneProposal:
			l.claimed[n.Path] = true
		case n.Output != nil && n.Output.Type == ir.OutputProposal:
			l.claimed[n.Path] = true
		}
	}
	// A selection's winner inherits its candidates' plan: whoever writes a
	// judge over a set of proposals has started talking about settlement, and
	// the proposal it picks is the one that still needs landing.
	for _, n := range l.order {
		if n.Kind != ir.KindSelect || l.claimed[n.Path] {
			continue
		}
		for _, id := range l.handleFor(n, n.Candidates).ids() {
			if l.claimed[id] {
				l.claimed[n.Path] = true
				break
			}
		}
	}
}

// tokensEverywhere lists every token the analysis met, in document order of
// the node that produces it, so a rule and the proof artifact agree on order.
func (l *linearity) tokensEverywhere() []string {
	var out []string
	for _, n := range l.order {
		if _, ok := l.seen[n.Path]; ok {
			out = append(out, n.Path)
		}
	}
	var orphan []string
	for id := range l.seen {
		if l.byPath(id) == nil {
			orphan = append(orphan, id)
		}
	}
	// A token with no producer node in the graph cannot happen through
	// Resolve; it can only come from a hand-edited graph whose paths were
	// rewritten. Ordered after the producers so the artifact stays stable.
	sort.Strings(orphan)
	return append(out, orphan...)
}

// byPath finds a node by its resolved path; token ids are node paths.
func (l *linearity) byPath(path string) *ir.Node {
	for _, n := range l.order {
		if n.Path == path {
			return n
		}
	}
	return nil
}

// inside reports whether a node sits in container's body, at any depth.
func (l *linearity) inside(node, container *ir.Node) bool {
	for up := l.parent[node]; up != nil; up = l.parent[up] {
		if up == container {
			return true
		}
	}
	return false
}

// reachable reports whether a run can get from one node to another. The
// pairwise rules ask it of two settlement steps: with no path either way they
// may both execute, which is what makes their path intent a question (WF023),
// and with a path from a skip-prone settle on to another settle, the skipped
// set has somewhere to be caught (WF024). The answer is about top-level steps,
// so a settle inside a body is reached through its container, which is also
// how the sibling order inside that body reads: two settlements in one body
// are ordered, because the body is a sequence.
func (l *linearity) reachable(from, to *ir.Node) bool {
	from, to = l.flow(from), l.flow(to)
	if from == nil || to == nil {
		return false
	}
	if from == to {
		return true
	}
	seen := map[*ir.Node]bool{from: true}
	stack := []*ir.Node{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, s := range l.succs[n] {
			if s == to {
				return true
			}
			if !seen[s] {
				seen[s] = true
				stack = append(stack, s)
			}
		}
	}
	return false
}

// ------------------------------------------------------------ the handle set

// handleFor resolves a selector that should denote a set of proposals, as
// written by reader. Three things are decided here and each is a rule's whole
// premise:
//
//   - which dispatch nodes the handle names, which is WF020's lane evidence
//     and WF021's bookkeeping;
//   - whether it can denote nothing at all, which is WF026's: a filter, a
//     run-time-sized fan-out, an empty literal list, and a value the graph
//     cannot trace are all possibly empty, and `on_empty` is the author's
//     answer for each;
//   - whether it denotes one proposal, which is WF024's exemption: a set of
//     one has no two elements to conflict, so `on_conflict: skip` on it owes
//     no catch.
func (l *linearity) handleFor(reader *ir.Node, sel *ir.Selector) proposalSet {
	if sel == nil || sel.From == nil {
		return proposalSet{maybeEmpty: true}
	}
	// A filter is the plain statement that the set may come back empty, and a
	// field-only clause is a retention filter in the same shape (pinned on
	// ir.Predicate.Field): it keeps elements, it does not guarantee any.
	filtered := sel.Predicate.Op != ir.PredNone || sel.Predicate.Field != ""
	out := proposalSet{
		singleton:  sel.AsList,
		maybeEmpty: sel.AsList || filtered,
	}
	switch sel.From.Kind {
	case ir.SourceStepCollect, ir.SourceStepOutput:
		target := l.scopeOf(reader)[sel.From.StepID]
		if target == nil {
			// referencesResolve reports the unresolvable step; nothing here
			// can be said about the set it would have denoted.
			out.maybeEmpty = true
			return out
		}
		if sel.From.Kind == ir.SourceStepOutput {
			out.attributed = true
			out.known = true
			out.referents = []*ir.Node{target}
			if !sel.AsList {
				// One step's output is one value; read as a set it is either
				// a list that value holds or WF011's finding, and either way
				// its size is not knowable here.
				out.maybeEmpty = true
			}
			return out
		}
		if len(target.Body) == 0 {
			out.maybeEmpty = true
			return out
		}
		out.attributed = true
		out.known = true
		out.referents = l.collectReferents(target)
		switch {
		case target.Over != nil && target.Over.Selector != nil:
			// A run-time item list can be empty and is not one element.
			out.maybeEmpty = true
			out.singleton = false
		case target.Over != nil && target.Over.Literals != nil:
			out.maybeEmpty = out.maybeEmpty || len(target.Over.Literals) == 0
			out.provablyEmpty = len(target.Over.Literals) == 0
			out.singleton = !filtered && len(target.Over.Literals) <= 1
		default:
			// A loop, or a container with no source at all: the count of
			// items nobody wrote down is not this package's to assume.
			out.maybeEmpty = true
			out.singleton = false
		}
	default:
		// A parameter or an item binding carries no lane and no size for this
		// package to read. WF020 stays silent about it and WF021 has no token
		// to count: the graph did not produce those proposals. A declared
		// default is the one case the document states outright.
		out.maybeEmpty = true
		out.singleton = false
		out.provablyEmpty = emptyDefault(l.g, sel.From)
	}
	return out
}

// emptyDefault reports a parameter whose declared default is a list with no
// elements — the only set size a `params:` source can state.
func emptyDefault(g *ir.Graph, src *ir.Source) bool {
	if g == nil || src == nil || src.Kind != ir.SourceParams || len(src.Path) > 0 {
		return false
	}
	p, ok := g.Params[src.Name]
	if !p.HasDefault {
		return false
	}
	list, isList := p.Default.([]any)
	return ok && isList && len(list) == 0
}

// collectReferents lists the dispatch nodes a container's collect denotes:
// its body's dispatches, reaching through a nested container only when that
// container publishes a collect of its own — which is the only way an outer
// step can read a nested value back.
func (l *linearity) collectReferents(container *ir.Node) []*ir.Node {
	var out []*ir.Node
	var walk func(nodes []*ir.Node)
	walk = func(nodes []*ir.Node) {
		for _, n := range nodes {
			if n == nil {
				continue
			}
			switch n.Kind {
			case ir.KindDelegate, ir.KindSelect:
				out = append(out, n)
			default:
				if len(n.Body) > 0 && n.Collect != "" {
					walk(n.Body)
				}
			}
		}
	}
	walk(container.Body)
	return out
}

// templateSource is the single step a seed/continue/fork template reads a
// proposal id from. The template renders text, so what matters for linearity
// is the step it names, and interpolating around the reference does not change
// which proposal that is. A template with no step reference or with two
// different ones denotes no single proposal, and the caller reports it.
func templateSource(t *ir.Template) *ir.Source {
	if t == nil {
		return nil
	}
	var found *ir.Source
	add := func(src *ir.Source) {
		if src == nil || (src.Kind != ir.SourceStepOutput && src.Kind != ir.SourceStepCollect) {
			return
		}
		if found == nil {
			found = src
			return
		}
		if found.Kind != src.Kind || found.StepID != src.StepID {
			found = nil // ambiguous: two different steps
		}
	}
	for _, r := range t.Refs {
		add(sourceOfRef(r))
	}
	for _, seg := range t.Segments {
		if seg.Ref == nil {
			continue
		}
		add(sourceOfRef(seg.Ref))
	}
	return found
}

// selectorOfSource wraps a compiled source as the one-field selector
// handleFor reads, so a template reference and a `proposals:` selector go
// through the same resolution. The wrapper is a fresh value; the graph is not
// touched.
func selectorOfSource(src *ir.Source) *ir.Selector {
	if src == nil {
		return nil
	}
	return &ir.Selector{From: src}
}

// ------------------------------------------------------------- state utils

func cloneState(in map[string]linearityState) map[string]linearityState {
	out := make(map[string]linearityState, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeInto(dst map[string]linearityState, src map[string]linearityState) {
	for id, s := range src {
		dst[id] = mergeState(dst[id], s)
	}
}

func sameState(a, b map[string]linearityState) bool {
	if len(a) != len(b) {
		return false
	}
	for id, s := range a {
		o, ok := b[id]
		if !ok || o.counts != s.counts || o.acts != s.acts || strings.Join(o.by, ",") != strings.Join(s.by, ",") {
			return false
		}
	}
	return true
}
