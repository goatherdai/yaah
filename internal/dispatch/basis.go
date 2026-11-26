package dispatch

// basis.go makes the dispatch basis recorded, durable, and legible (yaah-witj
// increment A). The basis is the kernel workspace state a dispatch's work is
// measured against — the state settlement compares the parent against, not the
// parent's HEAD at read time. Nothing here changes where a worktree is seeded
// from; it removes the state where the record and the fork point could
// disagree without anyone noticing.

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
)

// proposalBasisRecord is the durable, legible record of the workspace state a
// dispatch started from: the backend and the commit/tree identities, without
// the full kernel state. A repository-backed dispatch records all three; a
// container session's snapshot backend records the backend and its revision.
//
// The full state is still the authority wherever one is available (a sealed
// output's Baseline(), a container proposal's basis.json); this record is what
// survives in the seal metadata and in the trace's settle facts, so a restarted
// process or a post-settlement reader can still say what the work started from.
type proposalBasisRecord struct {
	Backend string `json:"backend,omitempty"`
	Head    string `json:"head,omitempty"`
	Tree    string `json:"tree,omitempty"`
	// Ref is the caller's own spelling of the ref this basis was seeded from,
	// recorded only when a dispatch named one with `base_ref` (yaah-witj
	// increment B). It sits BESIDE the resolved Head and Tree rather than
	// instead of them: Head and Tree are what anything is verified against,
	// while Ref is the historical record of what was ASKED for.
	//
	// It is persisted deliberately, and this is the decision a previous version
	// of this code reversed. That version stripped the ref from the durable
	// record on the grounds that it is request state and what it names moves —
	// true, and beside the point: what it names moving is exactly why an
	// envelope needs it. Without the spelling a reader sees basis_head that
	// differs from the repository's HEAD and nothing saying the difference was
	// REQUESTED, so a caller-named basis reads as unexplained drift. "The
	// caller asked for origin/feature-x" does not go stale when the ref moves;
	// re-resolving the name would, which is why nothing here ever re-resolves
	// it.
	//
	// It lives on the basis record rather than in a field of its own so the two
	// cannot be persisted in different places: every surface that names the
	// basis names the request with it (setBasis stamps both), which is what
	// keeps `proposal show`, `list` and the settlement envelopes from promising
	// less than the tool docs say they carry.
	Ref string `json:"ref,omitempty"`
}

// basisRecord renders one workspace state as a durable record. A zero state has
// no basis to record, so it answers nil: an absent record means "none was
// recorded", never "the basis is empty".
func basisRecord(state kerneltrace.WorkspaceState) *proposalBasisRecord {
	if state.Backend == "" {
		return nil
	}
	rec := &proposalBasisRecord{Backend: state.Backend, Head: state.Revision}
	if rec.Head == "" {
		// Revision is the git HEAD SHA for a git state; a hand-built state may
		// carry only the data field.
		rec.Head, _ = state.Data[workspaceStateHeadKey].(string)
	}
	rec.Tree, _ = state.Data[workspaceStateTreeKey].(string)
	return rec
}

// resolvedBasisRecord is the durable record of the state this fork's delta is
// measured against — the resolved baseline, not the requested one — carrying
// the caller's spelling of the ref it came from when the fork was seeded from
// one (see proposalBasisRecord.Ref).
//
// The spelling is stamped only when a basis exists at all: a ref with no
// recorded basis would put `base_ref` on an envelope with no `basis_head`, and
// the documented reading of the field is "a branch or tag name whose commit is
// basis_head" — a pairing, not a lone name.
func (s threadSeed) resolvedBasisRecord() *proposalBasisRecord {
	rec := basisRecord(s.resolvedBaseline())
	if rec != nil {
		rec.Ref = s.ref
	}
	return rec
}

// factFields renders the record as trace-fact keys. The settle facts use them so
// the basis stays legible after the proposal's own record is consumed — the
// ref spelling included, because a settlement is the last envelope a reader
// sees and an unexplained basis there is the same misreading one step later.
func (r *proposalBasisRecord) factFields() map[string]any {
	if r == nil {
		return nil
	}
	fields := map[string]any{"basis_backend": r.Backend}
	if r.Head != "" {
		fields["basis_head"] = r.Head
	}
	if r.Tree != "" {
		fields["basis_tree"] = r.Tree
	}
	if r.Ref != "" {
		fields["base_ref"] = r.Ref
	}
	return fields
}

// dispatchBasis is the state this proposal's delta is measured against,
// whichever lane produced it: a worktree proposal's sealed fork baseline, the
// container lane's persisted host basis, or a failed seal's recorded baseline.
func (p *retainedProposal) dispatchBasis() (kerneltrace.WorkspaceState, bool) {
	if p == nil {
		return kerneltrace.WorkspaceState{}, false
	}
	if p.output != nil {
		if baseline := p.output.Baseline(); baseline.Backend != "" {
			return baseline, true
		}
	}
	if p.basisState.Backend != "" {
		return p.basisState, true
	}
	if p.failedSeal != nil {
		// The artifact's recorded baseline, or its workspace when none was
		// recorded — resolvedBaseline's rule, which is also the rule the fork
		// itself applies (failedSealForkHandle), so what is REPORTED here and
		// what the continuation was MEASURED against cannot disagree.
		state := threadSeed{seed: p.failedSeal.Workspace, baseline: p.failedSeal.Baseline}.resolvedBaseline()
		if state.Backend != "" {
			return state, true
		}
	}
	return kerneltrace.WorkspaceState{}, false
}

// dispatchBasisRecord is dispatchBasis as a durable record. When no state is
// left to read — a proposal rehydrated in a later process whose sealed output no
// longer carries a baseline — the record the seal itself persisted still answers,
// because surviving the state is the whole reason that record exists.
//
// A nil proposal answers nil, matching dispatchBasis: the two are the state and
// its rendering, so a caller that inherits one method's nil-safety cannot fall
// into a panic by switching to the other.
func (p *retainedProposal) dispatchBasisRecord() *proposalBasisRecord {
	if p == nil {
		return nil
	}
	if state, ok := p.dispatchBasis(); ok {
		rec := basisRecord(state)
		// A live state says which commit and tree the work started from; it
		// cannot say that the caller NAMED the ref those came from, because a
		// kernel capture has no request in it. That half survives only in the
		// record the seal wrote (retainedProposalMeta.Basis), so the spelling is
		// carried over rather than left to a live-state-wins rule that would
		// silently drop it for every in-session proposal.
		if rec != nil {
			rec.Ref = p.basisRef
			if rec.Ref == "" && p.sealBasisRecord != nil {
				rec.Ref = p.sealBasisRecord.Ref
			}
		}
		return rec
	}
	return p.sealBasisRecord
}

// setBasis stamps the dispatch basis onto the envelope, together with the
// caller's spelling of the ref it was seeded from when one was named. Absent
// means none: a proposal with no recorded basis leaves every basis field out of
// the JSON rather than emitting them empty, so a reader can tell "not recorded"
// from "recorded as empty".
//
// Both halves are stamped HERE, in one place, and every surface that names a
// basis calls this: the dispatch envelope, show, list, the conflict response and
// every settlement. Stamping the ref anywhere else is what let the tool docs
// promise it on envelopes that never carried it (PR #428 review finding 1).
func (e *proposalEnvelope) setBasis(rec *proposalBasisRecord) {
	if rec == nil {
		return
	}
	e.BasisBackend, e.BasisHead, e.BasisTree = rec.Backend, rec.Head, rec.Tree
	e.BaseRef = rec.Ref
}

// setBasis stamps the dispatch basis onto one list summary, with the same
// absent-means-none rule (and the same ref spelling) as the envelope.
func (s *proposalSummary) setBasis(rec *proposalBasisRecord) {
	if rec == nil {
		return
	}
	s.BasisBackend, s.BasisHead, s.BasisTree = rec.Backend, rec.Head, rec.Tree
	s.BaseRef = rec.Ref
}

// basisDivergence reports that the repository the worktree will be created from
// and the state the dispatch's delta is measured against do not name the same
// commit. The two are read from different places — the worktree from the repo
// path, the basis from the parent scope's sandbox — so nothing but this check
// makes them agree. It answers the warning text and, for the durable copy, the
// two commits it is about; no finding means both are empty.
//
// It is a warning, not a refusal: the fork seed is applied to the child's own
// sandbox before the fork record, so the child's contents come from the recorded
// state either way, and every settlement path proves the parent against that
// same state by content before writing. What a divergence breaks is the
// reader's assumption that "the basis is the branch I am on", which is exactly
// the assumption a caller-specified fork point would invalidate — so it is
// recorded on the envelope and left for a human to act on, matching how a
// degraded dispatch world is reported.
func basisDivergence(ctx context.Context, repoPath string, state kerneltrace.WorkspaceState) (string, map[string]any) {
	if repoPath == "" || state.Backend != "git" {
		return "", nil
	}
	head := state.Revision
	if head == "" {
		head, _ = state.Data[workspaceStateHeadKey].(string)
	}
	if head == "" {
		return "", nil
	}
	repoHead, err := gitHeadSHA(ctx, repoPath)
	if err != nil || repoHead == "" || repoHead == head {
		// An unreadable HEAD is not evidence of divergence: the worktree
		// provisioning below fails loudly with its own error if the repo is not
		// usable, and a warning here would be noise on a guess.
		return "", nil
	}
	warning := fmt.Sprintf("dispatch basis %s differs from the worktree repository's HEAD %s: the worktree is checked out from HEAD and then seeded with the recorded basis, so the basis does not name the commit the worktree started from", head, repoHead)
	return warning, map[string]any{"basis_head": head, "repo_head": repoHead}
}

// SchemaProposalBasisDiverged records a dispatch whose recorded basis and
// worktree repository HEAD named different commits (yaah-witj increment A).
const SchemaProposalBasisDiverged = "yaah.proposal.basis_diverged.v1"

// recordBasisDivergenceFact appends a divergence finding to the dispatch's own
// scope, beside the world fact. The warning rides the envelope, which is
// ephemeral; a merely degraded world gets a durable fact (recordWorldFact) and
// the divergence is the same kind of caveat, so it gets the same treatment.
// Advisory: the dispatch has already been decided, so a failed append is a
// record gap, not a refusal.
func recordBasisDivergenceFact(ctx context.Context, mgr *goatherd.ScopeManager, scopeID string, payload map[string]any) {
	if mgr == nil || scopeID == "" || len(payload) == 0 {
		return
	}
	scope, ok := mgr.Get(scopeID)
	if !ok {
		return
	}
	if _, err := scope.AppendFact(ctx, SchemaProposalBasisDiverged, "basis", payload); err != nil {
		slog.Debug("basis fact: append failed", "scope", scope.ID(), "err", err)
	}
}

// gitHeadReadTimeout bounds one gitHeadSHA read. The read happens once per
// dispatch, during provisioning, on the dispatch's own context — which carries
// the run's wall-clock budget — so without a bound of its own a git process
// parked on a held index.lock or a credential prompt would spend that budget.
const gitHeadReadTimeout = 5 * time.Second

// gitHeadSHA reads a repository's HEAD commit, under a deadline of its own (see
// gitHeadReadTimeout).
//
// Every caller treats a failure as "no evidence of divergence" — basisDivergence
// only warns when it can read both commits — so a timeout degrades to a missing
// warning about a basis/repo mismatch, never to a failed dispatch. The bound is
// therefore cheap insurance rather than a correctness requirement.
func gitHeadSHA(ctx context.Context, repoPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitHeadReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
