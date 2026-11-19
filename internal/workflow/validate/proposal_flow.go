// Proposal linearity: flow relation construction and token propagation.

package validate

import (
	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// build lays out the external flow edges.
func (l *linearity) build() {
	for _, n := range l.order {
		for _, ref := range n.Needs {
			if dep, ok := l.top[ref]; ok && dep != n {
				l.edge(dep, n)
			}
		}
		for _, src := range l.reads(n) {
			dep := l.resolved(n, src)
			if dep == nil || dep == n {
				continue
			}
			l.edge(dep, n)
		}
		if n.Kind == ir.KindBranch || n.Kind == ir.KindGate {
			for _, arm := range l.arms(n) {
				l.edge(n, arm)
			}
		}
	}
	// A run finishes where the flow has nowhere left to go. Containers count
	// at their own boundary, because compute has already carried a body's
	// state out through them.
	for _, n := range l.g.Steps {
		if n == nil {
			continue
		}
		if len(l.succs[n]) == 0 {
			l.runEnds = append(l.runEnds, n)
			continue
		}
		// An arm set that cannot cover every outcome also ends the flow: a
		// branch with no default and a gate missing a disposition both have a
		// case where no arm runs, and that is the run in which a proposal
		// settled only inside an arm stays open.
		if len(l.arms(n)) > 0 && !l.armsTotal(n) {
			l.runEnds = append(l.runEnds, n)
		}
	}
}

// edge records one external flow edge in both directions, deduplicated.
//
// Both endpoints are lifted to their top-level step. A body node is entered
// through its container and sees the state the container's own transfer
// produced — a loop's continue_from, for one — so an edge that stopped at the
// body node would hand it a predecessor's state that had not passed through
// the container, and a proposal the container superseded would arrive there
// still open. Needs and arm targets already name top-level steps only
// (ir.resolveNode and resolveBranch enforce it); a data reference from inside
// a body may name one, and that is the case the lift is for.
func (l *linearity) edge(from, to *ir.Node) {
	from, to = l.flow(from), l.flow(to)
	if from == to || from == nil || to == nil {
		return
	}
	for _, n := range l.preds[to] {
		if n == from {
			return
		}
	}
	l.preds[to] = append(l.preds[to], from)
	l.succs[from] = append(l.succs[from], to)
}

// flow is the step a node's flow is attributed to: the top-level step that
// contains it, or the node itself. Everything the family says about ordering,
// concurrency, and where a run can end is a statement about top-level steps,
// because a body has no independent place in the schedule.
func (l *linearity) flow(n *ir.Node) *ir.Node {
	if n == nil {
		return nil
	}
	for l.parent[n] != nil {
		n = l.parent[n]
	}
	return n
}

// arms are a branch's or gate's alternative successors, resolved to nodes.
func (l *linearity) arms(n *ir.Node) []*ir.Node {
	var out []*ir.Node
	add := func(id string) {
		if id == "" {
			return
		}
		if t, ok := l.top[id]; ok {
			out = append(out, t)
		}
	}
	for _, c := range n.Cases {
		add(c.Then)
	}
	add(n.Default)
	add(n.OnPass)
	add(n.OnFail)
	return out
}

// armsTotal reports whether the declared arms cover every outcome: a branch
// with a default, a gate with both dispositions.
func (l *linearity) armsTotal(n *ir.Node) bool {
	switch n.Kind {
	case ir.KindBranch:
		return n.Default != ""
	case ir.KindGate:
		return n.OnPass != "" && n.OnFail != ""
	}
	return true
}

// reads returns every step source the node's own values name, deduplicated.
// Both halves of the value surface count: selectors, which are the graph's
// read-side, and compiled template references, which is how a prompt or a
// continue_from reaches back at a step.
func (l *linearity) reads(n *ir.Node) []*ir.Source {
	var out []*ir.Source
	add := func(src *ir.Source) {
		if src == nil {
			return
		}
		if src.Kind != ir.SourceStepOutput && src.Kind != ir.SourceStepCollect {
			return
		}
		for _, seen := range out {
			if seen.Kind == src.Kind && seen.StepID == src.StepID {
				return
			}
		}
		out = append(out, src)
	}
	sel := func(s *ir.Selector) {
		if s != nil {
			add(s.From)
		}
	}
	tpl := func(t *ir.Template) {
		if t == nil {
			return
		}
		for _, r := range t.Refs {
			add(sourceOfRef(r))
		}
		for _, seg := range t.Segments {
			add(sourceOfRef(seg.Ref))
		}
	}
	sel(n.When)
	sel(n.On)
	sel(n.Until)
	if n.Over != nil {
		sel(n.Over.Selector)
	}
	sel(n.Proposals)
	sel(n.Where)
	sel(n.Candidates)
	for _, s := range n.Inputs {
		sel(s)
	}
	tpl(n.Prompt)
	tpl(n.SeedFrom)
	tpl(n.ContinueFrom)
	tpl(n.ForkFrom)
	tpl(n.Value)
	if n.Judge != nil {
		tpl(n.Judge.Prompt)
	}
	for _, k := range sortedParamKeys(n.CallParams) {
		tpl(n.CallParams[k].Template)
	}
	return out
}

// resolved looks one step source up in the scope of the node that holds it.
func (l *linearity) resolved(n *ir.Node, src *ir.Source) *ir.Node {
	if src == nil {
		return nil
	}
	return l.scopeOf(n)[src.StepID]
}

// scopeOf is the reference namespace one node resolves against: every
// top-level step id, plus the body-local ids of every container it sits in,
// up to and including itself, exactly as ir.resolveBody registers them. This
// family rebuilds it rather than borrowing the value family's copy because
// every rule file here owns its own traversal.
func (l *linearity) scopeOf(n *ir.Node) map[string]*ir.Node {
	if s, ok := l.scope[n]; ok {
		return s
	}
	s := map[string]*ir.Node{}
	for id, node := range l.top {
		s[id] = node
	}
	for child := n; ; {
		up := l.parent[child]
		if up == nil {
			break
		}
		for _, sib := range up.Body {
			if sib == nil || sib.ID == "" {
				continue
			}
			s[sib.ID] = sib
			if sib == child {
				break
			}
		}
		child = up
	}
	l.scope[n] = s
	return s
}

// --------------------------------------------------------------- dataflow

// linearityRounds bounds the Kleene iteration. The flow relation is a DAG for
// any document the earlier families accept — needs is acyclic (WF003) and a
// node may not read its own output (WF013) — so one pass in document order
// converges, and the bound only matters for a hand-edited graph whose
// backward references close a cycle: the walk stops there instead of spinning.
func (l *linearity) rounds() int {
	if len(l.order) < 2 {
		return 2
	}
	return len(l.order) + 1
}

// propagate evaluates every node, container subtrees included, until the
// boundary states stop growing.
func (l *linearity) propagate() {
	l.markGuarded()
	for i := 0; i < l.rounds(); i++ {
		changed := false
		for _, n := range l.g.Steps {
			if n == nil {
				continue
			}
			if l.compute(n, nil) {
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	l.runExit = map[string]linearityState{}
	for _, n := range l.runEnds {
		mergeInto(l.runExit, l.exitState(n))
	}
	l.seen = map[string]linearityState{}
	for _, n := range l.order {
		for id, s := range l.exit[n] {
			l.seen[id] = mergeState(l.seen[id], s)
		}
	}
	for id, s := range l.runExit {
		l.seen[id] = mergeState(l.seen[id], s)
	}
}

// markGuarded records which nodes sit behind a branch arm or a gate
// disposition. The flag decides how a node's predecessors are combined, which
// is the difference between two readings of the same graph that only look
// right in one of the two cases: needs and data dependencies are *joins* — the
// scheduler waits for every one of them, so all of them ran, and a proposal
// settled by the later one cannot still be open at this node — while anything
// downstream of a guard is an *alternative*, so a proposal settled only in one
// arm really is left open on the path that took the other.
//
// The mark spreads to the whole downstream cone of an arm, which is the
// conservative direction: a guarded graph falls back to the may-union it would
// have had anyway, and only an unguarded chain gets the join that stops a
// settle's own state from being diluted by an ancestor it already waited for.
func (l *linearity) markGuarded() {
	work := []*ir.Node{}
	for _, n := range l.order {
		if n.Kind != ir.KindBranch && n.Kind != ir.KindGate {
			continue
		}
		for _, arm := range l.arms(n) {
			if !l.guarded[arm] {
				l.guarded[arm] = true
				work = append(work, arm)
			}
		}
	}
	for len(work) > 0 {
		n := work[len(work)-1]
		work = work[:len(work)-1]
		for _, s := range l.succs[n] {
			if !l.guarded[s] {
				l.guarded[s] = true
				work = append(work, s)
			}
		}
	}
}

// joinDefinite combines the state of a step the scheduler waited for. Both
// steps finished, so for each token the more advanced of the two readings wins:
// a proposal disposed of by either one is disposed of here. Verbs and disposer
// names still union, because both really happened.
func joinDefinite(dst map[string]linearityState, src map[string]linearityState) {
	for id, s := range src {
		cur := dst[id]
		out := mergeState(cur, s)
		out.counts = advanced(cur.counts, s.counts)
		dst[id] = out
	}
}

// advanced is the join of two disposition counts for two steps that both ran:
// any advancement beats "not yet", and a second advancement stays visible.
func advanced(a, b uint8) uint8 {
	if out := (a | b) & (countOnce | countTwice); out != 0 {
		return out
	}
	return countNone
}

// compute evaluates one node and, if it is a container, its whole body. in is
// the state arriving from the previous sibling in the same body, nil at the
// top of a container or at the start of a body. It reports whether any
// boundary state grew, which is what tells the caller the walk has settled.
func (l *linearity) compute(n *ir.Node, in map[string]linearityState) bool {
	entry := map[string]linearityState{}
	guarded := false
	for _, p := range l.preds[n] {
		if l.guarded[p] {
			guarded = true
			break
		}
	}
	if in != nil {
		if guarded {
			mergeInto(entry, in)
		} else {
			joinDefinite(entry, in)
		}
	}
	for _, p := range l.preds[n] {
		if guarded {
			mergeInto(entry, l.exitState(p))
			continue
		}
		joinDefinite(entry, l.exitState(p))
	}
	down := l.transfer(n, entry)
	out := down
	if len(n.Body) > 0 {
		prev := down
		for _, b := range n.Body {
			if b == nil {
				continue
			}
			l.compute(b, prev)
			prev = l.exit[b]
		}
		out = l.gate(n, prev)
	}
	l.entry[n] = entry
	changed := !sameState(l.exit[n], out)
	l.exit[n] = out
	return changed
}

// exitState is the state a node hands to whoever runs after it.
func (l *linearity) exitState(n *ir.Node) map[string]linearityState {
	if s := l.exit[n]; s != nil {
		return s
	}
	return map[string]linearityState{}
}

// gate is the container boundary. A body's proposals leave it through the
// container's collect and nowhere else, so a container that declares none
// closes over them: the tokens stop here, and any that arrived undisposed at
// this boundary can never be disposed anywhere, because no handle outside can
// name them. That is recorded as stranded for WF021 rather than passed on as
// an open token the exit rules would have no path to attribute.
func (l *linearity) gate(container *ir.Node, bodyEnd map[string]linearityState) map[string]linearityState {
	if container.Collect != "" || bodyEnd == nil {
		return bodyEnd
	}
	out := map[string]linearityState{}
	for id, s := range bodyEnd {
		node := l.byPath(id)
		if node != nil && l.inside(node, container) {
			if s.counts&countNone != 0 {
				l.stranded[node] = true
			}
			continue
		}
		out[id] = s
	}
	return out
}

// transfer computes one node's exit state from its entry.
func (l *linearity) transfer(n *ir.Node, in map[string]linearityState) map[string]linearityState {
	out := cloneState(in)
	switch n.Kind {
	case ir.KindSettle:
		act := actionOf(n.Action)
		for _, id := range l.handleFor(n, n.Proposals).ids() {
			out[id] = namedBy(out[id], n.Path, act)
		}
	case ir.KindSelect:
		// A selection decides its candidates: the rejected ones are not open
		// proposals any more, and the winner is a new one. The reading is a
		// decision, not a deduction, and it is the one the vocabulary
		// implies — a judge that disposed nothing would make every speculative
		// workflow leak each candidate it did not pick, which is not what a
		// pick is for. The winner still owes its own disposition.
		for _, id := range l.handleFor(n, n.Candidates).ids() {
			out[id] = namedBy(out[id], n.Path, dispSelect)
		}
	case ir.KindLoop, ir.KindDelegate:
		if id := l.consumed(n); id != "" {
			out[id] = namedBy(out[id], n.Path, dispSupersede)
		}
	}
	if produces(n) {
		// Each execution mints a fresh proposal, so the token's own history
		// restarts here: a loop-carried disposition belongs to a previous
		// instance, and counting it against this one would report a double
		// settlement the run never performs.
		out[n.Path] = freshState()
	}
	if n.When != nil {
		// The guard can be false, and then the node does nothing at all and
		// its entry flows straight out. Keeping that alternative is what lets
		// WF021 see a proposal whose only settle was skipped.
		for id, s := range in {
			out[id] = mergeState(out[id], s)
		}
	}
	return out
}
