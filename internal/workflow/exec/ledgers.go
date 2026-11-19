package exec

import (
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
)

// settlementLedger is the run's proposal bookkeeping, extracted from Run so
// the linearity invariants have one owner and can be exercised on their own.
//
// open holds proposals this run sealed and has not disposed of; claims the
// ones a settle node is working through; settled the ones that reached a
// disposition; abortAsked the permanent record of abort-time asks. Each map is
// keyed proposal id -> owning node path (or, for abortAsked, the verb asked).
// They live in the reducer because the guards they serve must hold under any
// event ordering: a double settle is refused from the ledger, not from a scan
// of the graph, and a run that would end with a proposal still open is refused
// by the close itself.
type settlementLedger struct {
	open       map[string]string
	claims     map[string]string
	settled    map[string]string
	abortAsked map[string]string
}

func newSettlementLedger() settlementLedger {
	return settlementLedger{
		open:       map[string]string{},
		claims:     map[string]string{},
		settled:    map[string]string{},
		abortAsked: map[string]string{},
	}
}

// openProposals lists the open ids in sorted order, so the reason a run
// refuses to end succeeded names the same list every time.
func (l *settlementLedger) openProposals() []string {
	out := make([]string, 0, len(l.open))
	for id := range l.open {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// detail renders the residue for the run's reason line.
func (l *settlementLedger) detail() string {
	ids := l.openProposals()
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if by := l.open[id]; by != "" {
			parts = append(parts, id+" ("+by+")")
			continue
		}
		parts = append(parts, id)
	}
	return strings.Join(parts, ", ")
}

// releaseClaims drops the claims a terminating settle node held on proposals
// it will not dispose of, so a node that failed halfway through its plan
// leaves the rest settle-able instead of walled off. Already-settled ids are
// never touched.
func (l *settlementLedger) releaseClaims(st *NodeState, path string) {
	if st == nil || st.SettlePlan == nil {
		return
	}
	for i := st.Settling; i < len(st.SettlePlan.Refs); i++ {
		id := st.SettlePlan.Refs[i].ProposalID
		if _, done := l.settled[id]; done {
			continue
		}
		if holder, ok := l.claims[id]; ok && holder == path {
			delete(l.claims, id)
		}
	}
}

// instanceLedger owns the fan-out instance and branch-routing relations.
// instances holds every body node one item minted, keyed by instance path;
// of lists a container's instance paths in creation order; itemInFlight is the
// nested max_parallel tally; arms is the routing relation of branches and
// gates. Run's dependency graph stays separate: an arm may be both a routing
// target and a dependency, and the two relations must not be conflated.
type instanceLedger struct {
	instances    map[string]*instance
	of           map[string][]string
	itemInFlight map[string]int
	arms         map[string]map[string]bool
}

func newInstanceLedger() instanceLedger {
	return instanceLedger{
		instances:    map[string]*instance{},
		of:           map[string][]string{},
		itemInFlight: map[string]int{},
		arms:         map[string]map[string]bool{},
	}
}

// itemMaxParallel returns the container's declared cap for an instance path,
// or zero when the path is not an instance. byPath is the run's resolved graph
// view, passed in so the ledger stays independently testable.
func (l *instanceLedger) itemMaxParallel(path string, byPath map[string]*ir.Node) int {
	inst := l.instances[path]
	if inst == nil {
		return 0
	}
	return maxParallelOf(byPath[inst.owner])
}

// itemSlotFree reports whether the fan-out that owns path has room under its
// nested cap. The tally counts in-flight dispatches, which is at least as
// strict as counting items: a body of several parallel nodes is throttled by
// dispatch, so the looser reading cannot exceed max_parallel either.
func (l *instanceLedger) itemSlotFree(path string, byPath map[string]*ir.Node) bool {
	max := l.itemMaxParallel(path, byPath)
	if max <= 0 {
		return true
	}
	return l.itemInFlight[l.instances[path].owner] < max
}

// registerArms records which branches and gates may decide about an arm. It is
// a routing relation, not a dependency: an arm may declare its branch as a
// need and then be both. Unknown or self-naming arms are skipped — the
// resolver refuses those in authored graphs.
func (l *instanceLedger) registerArms(n *ir.Node, byID map[string]*ir.Node) {
	if n == nil || (n.Kind != ir.KindBranch && n.Kind != ir.KindGate) {
		return
	}
	for _, id := range armIDs(n) {
		dep, ok := byID[id]
		if !ok || dep.Path == n.Path {
			continue
		}
		if l.arms[dep.Path] == nil {
			l.arms[dep.Path] = map[string]bool{}
		}
		l.arms[dep.Path][n.Path] = true
	}
}

// armRouting answers whether a node is selectable and whether any branch that
// names it has terminated. An arm named by two branches runs if EITHER selected
// it, which is the reading that cannot silently drop work.
func (l *instanceLedger) armRouting(path string, state map[string]*NodeState) (selected, undecided bool) {
	owners := l.arms[path]
	if len(owners) == 0 {
		return true, false
	}
	for b := range owners {
		st := state[b]
		if st == nil || !st.State.Terminal() {
			return false, true
		}
		if st.State == StateSucceeded && st.Selected == path {
			return true, false
		}
	}
	return false, false
}
