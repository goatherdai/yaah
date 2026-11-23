package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
	"github.com/goatherdai/yaah/internal/types"
)

// TestSealFailureWithArtifactIsRecoverable pins yaah-9nqf: a typed seal error
// carrying a runtime artifact must keep the worktree, surface a failed-seal
// proposal, and advertise recovery verbs instead of discarding the world.
func TestSealFailureWithArtifactIsRecoverable(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	const artifactID = "art:failed:1"
	const sessionID = "test-session"

	rt := RetainedRuntime{
		Runner:       writingRunner(t, "out.txt").run(),
		RepoPath:     repo,
		SessionID:    sessionID,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
		sealOutput: func(_ context.Context, scopeID string, _ ...goatherd.SealOptions) (*goatherd.RetainedOutput, error) {
			meta := retainedProposalMeta{
				ID:            "proposal:failed:1",
				SessionID:     sessionID,
				ParentScopeID: rootID,
				Result:        "partial work",
				RunErr:        "turn_budget",
			}
			metaData, err := json.Marshal(meta)
			if err != nil {
				t.Fatalf("marshal failed-seal metadata: %v", err)
			}
			transcriptData, err := json.Marshal([]json.RawMessage{})
			if err != nil {
				t.Fatalf("marshal failed-seal transcript: %v", err)
			}
			h := &goatherd.FailedSeal{
				ArtifactID: artifactID,
				OutputID:   "out:failed:1",
				ScopeID:    scopeID,
				OwnerID:    strings.TrimPrefix(scopeID, "scope:"),
				Transcript: transcriptData,
				Metadata:   metaData,
				Workspace:  kerneltrace.WorkspaceState{Backend: "git", Revision: "abc"},
				Baseline:   kerneltrace.WorkspaceState{Backend: "git", Revision: "base"},
				Changes:    []goatherd.ProposedChange{{Path: "out.txt", Kind: goatherd.ChangeCreate}},
				Diff:       "a reviewable diff",
				Stage:      "sealed_append",
				Reason:     "injected append failure",
			}
			return nil, &goatherd.SealError{
				Stage:      "sealed_append",
				ArtifactID: artifactID,
				Handle:     h,
				Err:        errors.New("injected append failure"),
			}
		},
	}

	res, err := StartRetainedRun(context.Background(), rt, "do the work", SubAgentParams{Role: "worker"}, 0, false)
	if err != nil {
		t.Fatalf("StartRetainedRun: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "seal_failed" {
		t.Fatalf("status = %q, want seal_failed (error: %s)", env.Status, env.Error)
	}
	if env.ProposalID != artifactID {
		t.Errorf("proposal_id = %q, want %q", env.ProposalID, artifactID)
	}
	if !contains(env.Next, "recover") || !contains(env.Next, "discard") {
		t.Errorf("next = %v, want recover and discard", env.Next)
	}
	if env.FailureClass != FailureClassSealFailed {
		t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassSealFailed)
	}
	if env.Recovery != nil && env.Recovery.Retryable {
		t.Errorf("recovery = %+v, want non-retryable (recover the artifact)", env.Recovery)
	}

	p, err := rt.Proposals.get(artifactID)
	if err != nil {
		t.Fatalf("failed-seal proposal not registered: %v", err)
	}
	if p.failedSeal == nil || p.status != "seal_failed" {
		t.Fatalf("proposal = %+v, want a failed-seal handle with status", p)
	}
	if got := nextVerbsFor(p); len(got) != 3 || got[0] != "recover" {
		t.Errorf("next verbs = %v, want %v", got, sealFailedVerbs())
	}
	if files := p.changedFiles(); len(files) != 1 || files[0] != "out.txt" {
		t.Errorf("changed files = %v, want [out.txt] from the handle", files)
	}
	if p.reviewDiff() != "a reviewable diff" {
		t.Errorf("review diff = %q, want the handle's diff", p.reviewDiff())
	}
	if p.workdir == "" {
		t.Error("failed-seal proposal has no workdir; the reaper must still spare its scope")
	}
	scope, ok := fx.mgr.Get(p.scopeID)
	if !ok || scope.State() != goatherd.ScopeActive {
		t.Errorf("worktree scope was discarded after a recoverable seal failure: ok=%v", ok)
	}
	if ids := rt.Proposals.PendingScopeIDs(); len(ids) != 1 || ids[0] != p.scopeID {
		t.Errorf("pending scopes = %v, want the retained scope to be spared", ids)
	}
}

// TestRecoverFailedSealProposal_ReseedsFromHandle exercises the full recovery
// path: a failed-seal proposal seeds a fresh worktree via ForkFromFailedSeal,
// runs, and seals a new proposal with its own durable transcript.
func TestRecoverFailedSealProposal_ReseedsFromHandle(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	ctx := context.Background()
	root, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	state, err := root.Sandbox().Capture(ctx)
	if err != nil {
		t.Fatalf("capture root workspace: %v", err)
	}
	transcript := []types.Message{{Role: "user", Content: "finish the task"}}
	transcriptData, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	metaData, err := json.Marshal(retainedProposalMeta{
		ID: "proposal:failed:recover", SessionID: "s", ParentScopeID: rootID, Result: "partial",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &goatherd.FailedSeal{
		ID: "art:recover:1", ArtifactID: "art:recover:1", OutputID: "out:recover:1",
		ScopeID: "scope:failed-source", OwnerID: "failed-source",
		Metadata: metaData, Transcript: transcriptData,
		Workspace: state, Baseline: state, Stage: "sealed_append", Reason: "injected",
	}
	reg := NewProposalRegistry()
	reg.add(&retainedProposal{
		id:         h.ArtifactID,
		scopeID:    h.ScopeID,
		transcript: transcript,
		params:     SubAgentParams{Role: "worker"},
		timeout:    2 * time.Minute,
		parentID:   rootID,
		mgr:        fx.mgr,
		repoPath:   repo,
		world:      "worktree",
		status:     "seal_failed",
		failedSeal: h,
	})
	rt := RetainedRuntime{
		Runner:       writingRunner(t, "out.txt").run(),
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    reg,
	}
	result, err := ContinueRetainedRun(ctx, rt, h.ArtifactID, 2, "finish", false)
	if err != nil {
		t.Fatalf("recover failed-seal proposal: %v", err)
	}
	env := decodeProposalEnvelope(t, result)
	if env.Status != "proposal" || env.ProposalID == h.ArtifactID {
		t.Fatalf("recovery envelope = %+v, want a new sealed proposal", env)
	}
	newProposal, err := reg.get(env.ProposalID)
	if err != nil {
		t.Fatalf("recovered proposal not registered: %v", err)
	}
	if newProposal.output == nil || newProposal.failedSeal != nil {
		t.Fatalf("recovered proposal = %+v, want a sealed output", newProposal)
	}
	if newProposal.output.DerivedFrom() != h.ArtifactID {
		t.Errorf("derived_from = %q, want %q", newProposal.output.DerivedFrom(), h.ArtifactID)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
