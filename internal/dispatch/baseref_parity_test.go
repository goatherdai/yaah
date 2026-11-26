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
