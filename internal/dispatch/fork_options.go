package dispatch

// fork_options.go is the only place this package builds a goatherd.ForkOptions,
// so a fork's contents (Seed) and the state its delta is measured against
// (Baseline) cannot be set independently of each other. "Only place" is
// enforced by a text tripwire, not by construction: the test
// TestForkOptionsHasOneConstructionSite scans this package's non-test files for
// the qualified construction form `goatherd.ForkOptions{` (type name AND opening
// brace), skipping comment lines, so a field-by-field assignment (declare a zero
// ForkOptions, then set Seed and Baseline separately) would evade it, and it
// never reads cmd/yaah or internal/agent. It catches the ordinary way the pair
// would come back, not every way. Comment lines are skipped so that this doc
// comment can quote the marker without satisfying it — a marker a comment can
// satisfy makes the scan's "the scan is stale, not green" guard unable to fire.
//
// The pairing is forced, not incidental. The runtime treats a zero Baseline as
// "capture the parent's workspace at fork time", so a fork that supplies a Seed
// and no Baseline applies one state to the child and measures its delta against
// another. Settlement trusts the recorded basis rather than the parent's HEAD:
// the apply fast path writes a proposal's post-images whole-path over any parent
// path it cannot prove moved (see driftedApplyPaths in git_delta.go and the
// three-way merge in proposal_merge.go). A child seeded from one state and
// measured against another therefore records a basis that describes the
// parent's branch, and applying it overwrites the parent's version of files that
// branch legitimately changed — silent data loss reported as "applied".

import (
	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
)

// freshForkSeed is the fork pair for a dispatch with no source proposal: the
// state the parent captured at dispatch is both what the child starts from and
// what its delta is measured against, so the recorded basis always describes
// the child's fork point.
func freshForkSeed(state kerneltrace.WorkspaceState) threadSeed {
	return threadSeed{seed: state, baseline: state}
}

// resolvedBaseline is the state this fork's delta is actually measured against:
// the recorded basis, or the seed when the recorded basis names no backend. A
// backend-less state cannot measure anything, so falling back to the seed keeps
// the delta incremental rather than host-time — the same semantics
// resolveThreadSeed documents for a source that recorded no basis (yaah-9m0c).
func (s threadSeed) resolvedBaseline() kerneltrace.WorkspaceState {
	if s.baseline.Backend == "" {
		return s.seed
	}
	return s.baseline
}

// forkOptionsFor builds the one legal fork-option set for a resolved seed.
//
// A threaded dispatch (seed_from/continue/fork) supplies two DELIBERATELY
// different states: the child is seeded with the source's sealed workspace (its
// work) and measures against the source's recorded basis, which is what keeps
// the cumulative delta stable across host drift (yaah-9m0c). What the pair may
// never be is a seed with no baseline: resolvedBaseline closes that here rather
// than at every call site, and the zero state still means "capture the parent's
// workspace at fork time" because a zero seed resolves to a zero baseline.
func (s threadSeed) forkOptionsFor() goatherd.ForkOptions {
	return goatherd.ForkOptions{Baseline: s.resolvedBaseline(), Seed: s.seed}
}

// failedSealForkHandle is the artifact handle a failed-seal fork must be given.
//
// ScopeManager.ForkFromFailedSeal takes no ForkOptions: it builds its own from
// the handle (`Baseline: h.Baseline, Seed: h.Workspace`) and refuses a handle
// with no workspace backend. So the handle IS the options pair on that path,
// and the A1 pairing is only preserved by resolving the pair before the call.
// An artifact whose baseline was never recorded therefore forks against its own
// workspace state — resolvedBaseline's rule — instead of being handed to the
// runtime with a zero baseline, which the runtime reads as "capture the parent
// at fork time" and would measure a seeded child's delta against a tree that
// child never started from. The returned copy is ours: the runtime only reads
// the handle, and the artifact's durable trace record is untouched.
//
// The copy is SHALLOW by design: h.Workspace.Data (and Baseline's, when one was
// recorded) is still the caller's map, aliased with the artifact's durable
// record. That is safe today because the only field this function replaces is
// Baseline and the runtime's fork path reads a handle without editing it. An
// editor who needs to change a state's Data in place — while resolving the pair
// or afterwards on the returned handle — must deep-copy the map first, or the
// edit reaches the artifact the trace record was built from.
func (s threadSeed) failedSealForkHandle() *goatherd.FailedSeal {
	if s.failedSeal == nil {
		return nil
	}
	h := *s.failedSeal
	h.Baseline = s.resolvedBaseline()
	return &h
}
