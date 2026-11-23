package exec

import (
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/trace"
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

// traceLedger is the run's written record: the canonical event log, the
// once-only warnings, and the typed trace projection (yaah-0u0v.17). They are
// one ledger because they are three renderings of one event sequence and must
// never disagree; none of them is read by a scheduler decision, which is why
// moving them off Run changes no behaviour.
//
// events is every committed log line; persisted the high-water mark the Store
// has already recorded; warnings the statements a partly-honored declaration
// produced; warned their keys; records the trace projection and
// recordsPersisted its high-water mark. Every projection method takes a
// traceView, so the whole ledger can be exercised without constructing a Run.
type traceLedger struct {
	events           []string
	persisted        int
	warnings         []string
	warned           map[string]bool
	records          []trace.Record
	recordsPersisted int
}

func newTraceLedger() traceLedger { return traceLedger{warned: map[string]bool{}} }

// warnOnce appends a run warning the first time a fact is observed and never
// again. Warnings are not part of the event log, so this cannot move a byte of
// the deterministic sequence; it exists so a declaration the run honored only
// partly is stated once instead of on every cycle that consults it.
func (l *traceLedger) warnOnce(key, msg string) {
	if l.warned == nil {
		l.warned = map[string]bool{}
	}
	if l.warned[key] {
		return
	}
	l.warned[key] = true
	l.warnings = append(l.warnings, msg)
}

// newEvents is the index of the first log line the store has not recorded. It is
// clamped so a caller that rewinds the marker cannot make the snapshot slice out
// of range.
func (l *traceLedger) newEvents() int {
	if l.persisted < 0 {
		return 0
	}
	if l.persisted > len(l.events) {
		return len(l.events)
	}
	return l.persisted
}

// newRecords is newEvents for the trace projection: the index of the first
// record the store has not recorded, clamped the same way.
func (l *traceLedger) newRecords() int {
	if l.recordsPersisted < 0 {
		return 0
	}
	if l.recordsPersisted > len(l.records) {
		return len(l.records)
	}
	return l.recordsPersisted
}

// traceView is the immutable graph and state view one trace record is built
// from, and the ledger's whole input: the projection reads no lifecycle fact the
// fold of the same event is about to change, which is what lets a record be
// built before the switch and disagree with nothing.
type traceView struct {
	byPath    map[string]*ir.Node
	state     map[string]*NodeState
	instances map[string]*instance
	runID     string
	specHash  string
}

// joinLedger owns the join decisions: which item of a fan-out is residue, which
// proposal a non-winning item left for the run to discard, and the write-ahead
// record of the discard ask.
//
// It holds no maps of its own on purpose. The join's per-container facts —
// items, winners, discards, the outstanding ask — are lifecycle state on
// NodeState, and Apply is the one mutator that may change them, so a copy here
// would be a second answer to the same question. What the ledger owns is the
// READING of those facts, which is the cohesive unit: the four questions the
// reducer and the driver ask a decided join, answerable from a hand-built state
// map with no Run in sight.
type joinLedger struct{}

// joinView is the state a join reading needs: the graph, the node states, the
// instance relation an item's owner comes from, and the proposal ledger a
// discard is derived from.
type joinView struct {
	byPath    map[string]*ir.Node
	state     map[string]*NodeState
	instances map[string]*instance
	proposals *settlementLedger
}

// readyLedger is the launch queue: node paths in canonical order, which is what
// makes the launch order a function of the document rather than of the order
// events happened to arrive in.
type readyLedger struct{ ready []string }

// insertReady queues a path in canonical position, doing nothing when it is
// already queued.
func (l *readyLedger) insertReady(path string) {
	i := sort.SearchStrings(l.ready, path)
	if i < len(l.ready) && l.ready[i] == path {
		return
	}
	l.ready = append(l.ready, "")
	copy(l.ready[i+1:], l.ready[i:])
	l.ready[i] = path
}

// dropReady removes a queued path, doing nothing when it is not there.
func (l *readyLedger) dropReady(path string) {
	i := sort.SearchStrings(l.ready, path)
	if i < len(l.ready) && l.ready[i] == path {
		l.ready = append(l.ready[:i], l.ready[i+1:]...)
	}
}

// allPaths lists every top-level path and, recursively, every instance beneath
// it, in canonical order, so a graph yields one order everywhere and no scan can
// quietly miss an instance. paths is the run's top-level order, passed in so the
// ledger stays independently testable.
func (l *instanceLedger) allPaths(paths []string) []string {
	out := make([]string, 0, len(paths)+len(l.instances))
	var emit func(path string)
	emit = func(path string) {
		out = append(out, path)
		for _, ip := range l.of[path] {
			emit(ip)
		}
	}
	for _, p := range paths {
		emit(p)
	}
	return out
}
