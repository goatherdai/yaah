package dispatch

// baseref_parity_test.go covers the two gaps PR #428 review found in the
// base_ref surface: the caller's ref spelling must survive on EVERY surface that
// names the basis (dispatch, show, list, settlement — it used to be set on the
// dispatch envelope only), and the advertised report-lane path — a reviewer
// dispatched against a branch — must actually seed, not merely be refused.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/recovery"
)

// TestRetained_BaseRefSurvivesOnEveryBasisSurface pins where the spelling
// appears. Before this, only the dispatch envelope carried base_ref, so `show`,
// `list` and settlement reported basis_head differing from the parent's HEAD
// with nothing saying the difference was requested — the unexplained-drift
// reading the tool docs promise against.
func TestRetained_BaseRefSurvivesOnEveryBasisSurface(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	rt := RetainedRuntime{
		Runner: func(_ context.Context, _ string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-surfaces",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	dispatched := decodeProposalEnvelope(t, res)
	if dispatched.Status != "proposal" {
		t.Fatalf("status = %q (%s)", dispatched.Status, dispatched.Error)
	}
	// The spelling is the caller's, not the resolved sha, and the basis is the
	// commit it named.
	if dispatched.BaseRef != "side" || dispatched.BasisHead != side {
		t.Fatalf("dispatch base_ref/basis_head = %q/%q, want \"side\"/%q", dispatched.BaseRef, dispatched.BasisHead, side)
	}
	// The live proposal's own copy. A kernel capture carries no request and a
	// live in-session proposal has no sealBasisRecord (that field is populated
	// only on rehydration), so this is the field that must hold the spelling
	// for a proposal that is still open — the one show/list/settle read.
	live, lerr := rt.Proposals.get(dispatched.ProposalID)
	if lerr != nil {
		t.Fatalf("live proposal: %v", lerr)
	}
	if live.basisRef != "side" {
		t.Fatalf("live proposal basisRef = %q, want the caller's spelling %q", live.basisRef, "side")
	}

	tool := &ProposalTool{Proposals: rt.Proposals}
	list, err := tool.Execute(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, `"base_ref":"side"`) {
		t.Fatalf("list does not name the ref the basis came from:\n%s", list)
	}
	show, err := tool.Execute(context.Background(), `{"action":"show","proposal_id":"`+dispatched.ProposalID+`"}`)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(show, `"base_ref":"side"`) {
		t.Fatalf("show does not name the ref the basis came from:\n%s", show)
	}
	settled, err := tool.Execute(context.Background(), `{"action":"apply","proposal_id":"`+dispatched.ProposalID+`"}`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(settled, `"base_ref":"side"`) {
		t.Fatalf("settlement does not name the ref the basis came from:\n%s", settled)
	}
	if _, serr := os.Stat(filepath.Join(repo, "work.txt")); serr != nil {
		t.Fatalf("apply did not land the proposal's file: %v", serr)
	}
}

// TestRetained_BaseRefSpellingSurvivesRestart is the REHYDRATION twin of
// TestRetained_BaseRefSurvivesOnEveryBasisSurface above, and it exists because
// that test structurally cannot see the durable half of the spelling.
//
// A live in-session proposal reports the ref from retainedProposal.basisRef, so
// `show`, `list` and settlement keep saying "side" even when the seal record
// never recorded the spelling: `live.basisRef` masks it. The behaviour
// increment B promises is the one AFTER a restart — a proposal rehydrated from
// the seal state has no live basisRef and no live dispatch state, so its
// spelling can only come from retainedProposalMeta.Basis. The mutation this
// test pins is therefore the deletion of `record.Ref = thread.basis.ref` from
// resolvedBasisRecord() (plus its factFields/dispatchBasisRecord plumbing): the
// surface test above stays green under it, and this one must not.
func TestRetained_BaseRefSpellingSurvivesRestart(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	rt := RetainedRuntime{
		Runner: func(_ context.Context, _ string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-restart",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	dispatched := decodeProposalEnvelope(t, res)
	if dispatched.Status != "proposal" {
		t.Fatalf("status = %q (%s)", dispatched.Status, dispatched.Error)
	}
	if dispatched.BaseRef != "side" || dispatched.BasisHead != side {
		t.Fatalf("dispatch base_ref/basis_head = %q/%q, want \"side\"/%q", dispatched.BaseRef, dispatched.BasisHead, side)
	}

	// The durable half, asserted on its own so a failure names the seal record
	// rather than a surface that could be reporting it from live state.
	live, lerr := rt.Proposals.get(dispatched.ProposalID)
	if lerr != nil {
		t.Fatalf("live proposal: %v", lerr)
	}
	sealed, _, merr := proposalMetaFromOutput(live.output)
	if merr != nil {
		t.Fatalf("sealed proposal metadata: %v", merr)
	}
	if sealed.Basis == nil {
		t.Fatal("the seal persisted no basis record")
	}
	if sealed.Basis.Ref != "side" {
		t.Fatalf("sealed basis ref = %q, want the caller's spelling %q", sealed.Basis.Ref, "side")
	}
	if sealed.Basis.Head != side {
		t.Fatalf("sealed basis head = %q, want the base_ref commit %q", sealed.Basis.Head, side)
	}

	// A real restart: rebuild the scope tree and its sealed outputs from the
	// durable trace, then rebuild the registry from that manager. The live
	// retainedProposal is gone with the old process; only the seal survives.
	newMgr, _, rerr := recovery.Recover(context.Background(), fx.store, repo, true, nil)
	if rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	reg, warnings := LoadProposalRegistry(repo, newMgr, rootID, "sess-baseref-restart")
	p, gerr := reg.get(dispatched.ProposalID)
	if gerr != nil {
		t.Fatalf("rehydrated proposal %s missing: %v (warnings: %v)", dispatched.ProposalID, gerr, warnings)
	}
	// The two live sources are gone by construction, which is what makes every
	// assertion below a statement about the seal record: basisRef was never
	// populated by rehydration, and the seal strips BaseRef from the params
	// template because a ref name there would be re-resolved by a continuation.
	if p.basisRef != "" {
		t.Fatalf("rehydrated proposal carries a live basisRef %q; the durable route would then be untested", p.basisRef)
	}
	if p.params.BaseRef != "" {
		t.Fatalf("rehydrated params carry base_ref %q; a ref name in the params template is re-resolved, not recorded", p.params.BaseRef)
	}
	if p.sealBasisRecord == nil || p.sealBasisRecord.Ref != "side" {
		t.Fatalf("rehydrated seal basis record = %+v, want the caller's spelling %q", p.sealBasisRecord, "side")
	}
	rec := p.dispatchBasisRecord()
	if rec == nil || rec.Ref != "side" || rec.Head != side || rec.Backend != "git" {
		t.Fatalf("rehydrated basis record = %+v, want the caller's spelling %q at the commit %q", rec, "side", side)
	}

	// Every surface a post-restart reader has: list, show, and settlement. Both
	// halves are asserted on each — the spelling is the point of this test, and
	// a name with no commit beside it would be a name nothing can verify.
	assertBasis := func(surface, out string) {
		t.Helper()
		if !strings.Contains(out, `"base_ref":"side"`) {
			t.Errorf("%s does not name the ref the basis came from:\n%s", surface, out)
		}
		if !strings.Contains(out, `"basis_head":"`+side+`"`) {
			t.Errorf("%s does not name the commit the basis resolved to:\n%s", surface, out)
		}
	}
	tool := &ProposalTool{Proposals: reg}
	list, err := tool.Execute(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	assertBasis("rehydrated list", list)
	show, err := tool.Execute(context.Background(), `{"action":"show","proposal_id":"`+dispatched.ProposalID+`"}`)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	assertBasis("rehydrated show", show)
	settled, err := tool.Execute(context.Background(), `{"action":"apply","proposal_id":"`+dispatched.ProposalID+`"}`)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertBasis("rehydrated settlement", settled)
	if _, serr := os.Stat(filepath.Join(repo, "work.txt")); serr != nil {
		t.Fatalf("apply did not land the rehydrated proposal's file: %v", serr)
	}
}

// TestRetained_BaseRefSeedsAReportWorld is the positive twin of the report-lane
// refusals: a reviewer dispatched against a branch must actually run at that
// commit. Every other report-lane base_ref test asserts a refusal, so a
// regression that dropped the seed for reports while proposals kept working
// went uncaught.
func TestRetained_BaseRefSeedsAReportWorld(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	var seenHead string
	var ran bool
	rt := RetainedRuntime{
		Runner: func(_ context.Context, _ string, params SubAgentParams) (string, error) {
			ran = true
			seenHead = mustGitIn(t, params.Workdir, "rev-parse", "HEAD")
			if _, err := os.Stat(filepath.Join(params.Workdir, "moved-on.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the report world holds moved-on.txt (err=%v); it must be seeded at the ref, not at HEAD", err)
			}
			return "the report", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-report",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "inspect", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, true)
	if err != nil {
		t.Fatalf("report dispatch: %v", err)
	}
	if !ran {
		t.Fatalf("the report body never ran: %q", res)
	}
	if seenHead != side {
		t.Fatalf("report world HEAD = %q, want the base_ref commit %q", seenHead, side)
	}
	if !strings.Contains(res, "the report") {
		t.Fatalf("result = %q, want the report text", res)
	}
}
