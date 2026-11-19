// Settlement: resolving proposal handles from run state.

package exec

import (
	"fmt"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// resolveHandles turns a settle node's proposals and where selectors into the
// refs it disposes of, and answers three things at once: whether the node may
// be planned yet (a producer that has not terminated is a wait, not an empty
// set), what the set is, and whether the declaration is something this bead
// can execute at all.
func (r *Run) resolveHandles(n *ir.Node) (refs []ProposalRef, deferred bool, refusal string) {
	if n.Proposals == nil || n.Proposals.From == nil {
		return nil, false, ReasonSettleSourceUnavailable + ": node declares no proposals handle set"
	}
	for _, sel := range []*ir.Selector{n.Proposals, n.Where} {
		if sel == nil || sel.From == nil {
			continue
		}
		if p := r.awaitingProducer(sel); p != "" {
			return nil, true, ""
		}
	}
	refs, refusal = r.handleRefs(n.Proposals)
	if refusal != "" {
		return nil, false, refusal
	}
	if n.Where != nil && n.Where.From != nil {
		if !sameSource(n.Proposals.From, n.Where.From) {
			return nil, false, ReasonSettleSourceUnavailable + ": where reads " + n.Where.From.String() +
				" while proposals reads " + n.Proposals.From.String() +
				"; correlating two steps' sets needs the join across two collects, which the scheduler does not perform yet"
		}
		filtered, why := applyFilter(refs, n.Where)
		if why != "" {
			return nil, false, why
		}
		refs = filtered
	}
	sortRefs(refs, n.Order)
	if n.Order == ir.OrderSmallestDiffFirst && len(refs) > 1 {
		r.warnOnce("settle-order:"+n.Path, fmt.Sprintf(
			"%s: order: smallest_diff_first settled in canonical proposal order — the scheduler holds no diff-size fact to order on", n.Path))
	}
	return refs, false, ""
}

// awaitingProducer returns the path of a step the selector reads that has not
// terminated yet, or "" when there is nothing to wait for.
func (r *Run) awaitingProducer(sel *ir.Selector) string {
	if sel == nil || sel.From == nil {
		return ""
	}
	if sel.From.Kind != ir.SourceStepOutput && sel.From.Kind != ir.SourceStepCollect {
		return ""
	}
	dep, ok := r.byID[sel.From.StepID]
	if !ok {
		return ""
	}
	if r.state[dep.Path].State.Terminal() {
		return ""
	}
	return dep.Path
}

// handleRefs resolves a proposals selector into proposal refs.
func (r *Run) handleRefs(sel *ir.Selector) ([]ProposalRef, string) {
	src := sel.From
	if src.Kind == ir.SourceItem {
		return nil, ReasonSettleSourceUnavailable + ": " + src.String() +
			" is an item binding inside a container body; a settle that reads its own item's scope is not in the scheduler core yet"
	}
	v, err := r.selectorValue(sel)
	if err != nil {
		return nil, ReasonSettleSourceUnavailable + ": " + err.Error()
	}
	if sel.AsList {
		v = []any{v}
	}
	refs, why := refsFromValue(v)
	if why != "" {
		return nil, ReasonSettleSourceUnavailable + ": " + src.String() + " " + why
	}
	// A set read from a step — its own output or a fan-out's collected items —
	// names that step as the producer of any element that did not record one.
	// A fan-out collect is the case this matters for: its elements are the
	// items' recorded values, and the container is the node the run holds
	// responsible for them.
	if src.Kind == ir.SourceStepOutput || src.Kind == ir.SourceStepCollect {
		if dep, ok := r.byID[src.StepID]; ok {
			for i := range refs {
				if refs[i].Producer == "" {
					refs[i].Producer = dep.Path
				}
			}
		}
	}
	// One id settled twice inside one plan is the same bug as two nodes
	// settling it, and the author wrote it in one expression.
	return dedupeRefs(refs), ""
}
