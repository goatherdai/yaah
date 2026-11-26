package dispatch

// baseref_parity_test.go covers the two gaps PR #428 review found in the
// base_ref surface: the caller's ref spelling must survive on EVERY surface that
// names the basis (dispatch, show, list, settlement — it used to be set on the
// dispatch envelope only), and the advertised report-lane path — a reviewer
// dispatched against a branch — must actually seed, not merely be refused.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
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

// TestRetained_BaseRefSpellingSurvivesContinuation is the CONTINUATION twin of
// the surface and restart tests above, and it pins the one route neither of
// them can see: `proposal continue` / `proposal fork` of a base_ref dispatch.
//
// A continuation clears `BaseRef` from the copied params on purpose (a ref name
// there would be resolved AGAIN, against a repository that may have moved), so
// the only way its envelope can still name the request is
// resolveThreadSeed copying the source's recorded spelling into the seed. This
// test therefore MOVES the `side` branch between the dispatch and the
// continuation: without that, "the recorded basis was reused" and "the name was
// re-resolved" would produce the same commit and the test could not tell them
// apart. Deleting the `ts.ref = rec.Ref` copy in resolveThreadSeed must fail
// here (mutation-verified; see the PR discussion).
func TestRetained_BaseRefSpellingSurvivesContinuation(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	var calls int
	var contParams SubAgentParams
	var contHead string
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, _ string, params SubAgentParams) (string, error) {
			calls++
			if calls == 1 {
				writeTestFile(t, params.Workdir, "work.txt", "work\n")
				jobs.CaptureSubAgentTranscript(ctx, []types.Message{
					types.SystemMsg("sys"),
					types.UserMsg("do the work"),
				})
				return "first result", nil
			}
			contParams = params
			contHead = mustGitIn(t, params.Workdir, "rev-parse", "HEAD")
			writeTestFile(t, params.Workdir, "more.txt", "more\n")
			jobs.CaptureSubAgentTranscript(ctx, []types.Message{
				types.SystemMsg("sys"),
				types.UserMsg("do the work"),
				types.UserMsg("continue"),
			})
			return "second result", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-continuation",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	orig := decodeProposalEnvelope(t, res)
	if orig.Status != "proposal" {
		t.Fatalf("status = %q (%s)", orig.Status, orig.Error)
	}
	if orig.BaseRef != "side" || orig.BasisHead != side {
		t.Fatalf("dispatch base_ref/basis_head = %q/%q, want \"side\"/%q", orig.BaseRef, orig.BasisHead, side)
	}

	// Move the branch the caller named. A continuation that re-resolved "side"
	// would now start at moved — a different commit from the one its delta is
	// measured against. Written with commit-tree + update-ref so the parent
	// repository's HEAD and working tree, which the continuation's fork depends
	// on, are untouched.
	tree := mustGitIn(t, repo, "rev-parse", side+"^{tree}")
	moved := mustGitIn(t, repo, "commit-tree", tree, "-p", side, "-m", "side moves on")
	mustGitIn(t, repo, "update-ref", "refs/heads/side", moved)
	if got := revParseTest(t, repo, "side"); got != moved || moved == side {
		t.Fatalf("side = %q, want the moved commit %q (the original basis was %q)", got, moved, side)
	}

	res2, err := ContinueRetainedRun(context.Background(), rt, orig.ProposalID, 3, "", false)
	if err != nil {
		t.Fatalf("ContinueRetainedRun: %v", err)
	}
	cont := decodeProposalEnvelope(t, res2)
	if cont.Status != "proposal" {
		t.Fatalf("continuation status = %q, want proposal (%s)", cont.Status, cont.Error)
	}
	// The continuation's world is the ORIGINAL basis commit, not the moved tip:
	// this is the assertion the moved branch exists to give teeth to.
	if contHead != side {
		t.Errorf("continuation world HEAD = %q, want the original base_ref commit %q (the moved tip is %q)", contHead, side, moved)
	}
	// The continuation is a dispatch of its own, so its params must NOT carry a
	// ref name a later run could resolve again; its basis comes from the seed.
	if contParams.BaseRef != "" {
		t.Errorf("continuation params BaseRef = %q, want empty (a ref name here would be re-resolved)", contParams.BaseRef)
	}
	if cont.BasisHead != side {
		t.Fatalf("continuation basis_head = %q, want the original base_ref commit %q (a re-resolved \"side\" would be %q)", cont.BasisHead, side, moved)
	}
	if cont.BaseRef != "side" {
		t.Fatalf("continuation base_ref = %q, want the caller's spelling %q carried through the seed", cont.BaseRef, "side")
	}
	// The live continuation proposal carries it too, so show/list/settle after
	// the continuation report the request rather than unexplained drift.
	live, lerr := rt.Proposals.get(cont.ProposalID)
	if lerr != nil {
		t.Fatalf("continuation proposal: %v", lerr)
	}
	if live.basisRef != "side" {
		t.Errorf("continuation proposal basisRef = %q, want %q", live.basisRef, "side")
	}
	if rec := live.dispatchBasisRecord(); rec == nil || rec.Ref != "side" || rec.Head != side {
		t.Errorf("continuation basis record = %+v, want ref %q at the original commit %q", rec, "side", side)
	}
	for _, want := range []string{"work.txt", "more.txt"} {
		if !slices.Contains(cont.Files, want) {
			t.Errorf("continuation files %v missing %q (seeded work + new work)", cont.Files, want)
		}
	}
}

// TestRetained_BaseRefSpellingSurvivesFailedSealContinuation covers the SECOND
// continuation route: recovering a failed seal. That branch of
// ContinueRetainedRun cannot call resolveThreadSeed (a failed seal has no sealed
// output), so it reads the spelling off p.dispatchBasisRecord and copies it into
// the seed itself — a separate statement of the same rule, and therefore
// separately able to regress.
//
// The failed seal is produced the way the lane's own tests produce one: an
// injected sealOutput returns a typed SealError carrying the artifact handle, so
// the registry entry is built by the production rehydration path
// (newFailedSealProposal). Only the first call fails; the continuation's own
// seal goes to the real ScopeManager, so the recovery is a full one. The
// handle's metadata is the seal's own bytes (SealOptions.Metadata), which is
// where the durable spelling lives.
func TestRetained_BaseRefSpellingSurvivesFailedSealContinuation(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)
	tree := mustGitIn(t, repo, "rev-parse", side+"^{tree}")
	// The artifact's recorded state, in the shape resolveBaseRef synthesizes:
	// a seed the continuation can actually fork from, at the base_ref commit.
	state := kerneltrace.WorkspaceState{
		Backend:  "git",
		Revision: side,
		Data:     map[string]any{workspaceStateHeadKey: side, workspaceStateTreeKey: tree},
	}

	var calls int
	var sealCalls int
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, _ string, params SubAgentParams) (string, error) {
			calls++
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			jobs.CaptureSubAgentTranscript(ctx, []types.Message{
				types.SystemMsg("sys"),
				types.UserMsg("do the work"),
			})
			return "partial work", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-failed-seal-continuation",
		Proposals:    NewProposalRegistry(),
	}
	rt.sealOutput = func(ctx context.Context, scopeID string, opts ...goatherd.SealOptions) (*goatherd.RetainedOutput, error) {
		sealCalls++
		if sealCalls > 1 {
			return fx.mgr.Seal(ctx, scopeID, opts...)
		}
		transcript, terr := json.Marshal([]types.Message{
			types.SystemMsg("sys"),
			types.UserMsg("do the work"),
		})
		if terr != nil {
			t.Fatalf("marshal artifact transcript: %v", terr)
		}
		if len(opts) == 0 {
			t.Fatal("seal called with no options; the artifact metadata is unavailable")
		}
		h := &goatherd.FailedSeal{
			ArtifactID: "art:baseref:1",
			OutputID:   "out:baseref:1",
			ScopeID:    scopeID,
			OwnerID:    strings.TrimPrefix(scopeID, "scope:"),
			Transcript: transcript,
			// The seal's own metadata bytes: the durable basis record (and its
			// ref spelling) rides there, exactly as it does on a real seal.
			Metadata:  opts[0].Metadata,
			Workspace: state,
			Baseline:  state,
			Stage:     "sealed_append",
			Reason:    "injected append failure",
		}
		return nil, &goatherd.SealError{
			Stage:      "sealed_append",
			ArtifactID: h.ArtifactID,
			Handle:     h,
			Err:        errors.New("injected append failure"),
		}
	}

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	failed := decodeProposalEnvelope(t, res)
	if failed.Status != EnvelopeStatusSealFailed {
		t.Fatalf("status = %q, want seal_failed (%s)", failed.Status, failed.Error)
	}
	if failed.BaseRef != "side" || failed.BasisHead != side {
		t.Fatalf("failed-seal envelope base_ref/basis_head = %q/%q, want \"side\"/%q", failed.BaseRef, failed.BasisHead, side)
	}

	// Move the branch, so a re-resolved name would pick a different commit.
	moved := mustGitIn(t, repo, "commit-tree", tree, "-p", side, "-m", "side moves on")
	mustGitIn(t, repo, "update-ref", "refs/heads/side", moved)
	if got := revParseTest(t, repo, "side"); got != moved || moved == side {
		t.Fatalf("side = %q, want the moved commit %q (the original basis was %q)", got, moved, side)
	}

	res2, err := ContinueRetainedRun(context.Background(), rt, failed.ProposalID, 3, "", true)
	if err != nil {
		t.Fatalf("ContinueRetainedRun on the failed seal: %v", err)
	}
	cont := decodeProposalEnvelope(t, res2)
	if cont.Status != "proposal" {
		t.Fatalf("recovered status = %q, want proposal (%s)", cont.Status, cont.Error)
	}
	if cont.BasisHead != side {
		t.Fatalf("recovered basis_head = %q, want the original base_ref commit %q (a re-resolved \"side\" would be %q)", cont.BasisHead, side, moved)
	}
	if cont.BaseRef != "side" {
		t.Fatalf("recovered base_ref = %q, want the caller's spelling %q carried through the artifact's basis record", cont.BaseRef, "side")
	}
	if calls != 2 {
		t.Errorf("sub-agent runs = %d, want 2 (the failed dispatch and the recovery)", calls)
	}
	if !slices.Contains(cont.Files, "work.txt") {
		t.Errorf("recovered files %v missing work.txt", cont.Files)
	}
}
