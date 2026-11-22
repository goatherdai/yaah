package dispatch

// retained_durable_test.go covers durable worktree proposals: after a simulated
// restart the kernel recovers the scope + sealed output from the trace, and the
// output's snapshot/metadata restore the result, transcript, and params so the
// proposal is listable, settle-able, and continuable.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
)

// TestRetained_RehydrateScopesBySession pins the session boundary: a proposal
// from another session is not surfaced even when it lives in the same repo.
func TestRetained_RehydrateScopesBySession(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	seed := []types.Message{types.UserMsg("go")}
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			jobs.CaptureSubAgentTranscript(ctx, seed)
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-a",
		Proposals:    NewProposalRegistry(),
	}

	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeProposalEnvelope(t, res)

	newMgr, _, rerr := recovery.Recover(context.Background(), fx.store, repo, true, nil)
	if rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	regB, _ := LoadProposalRegistry(repo, newMgr, rootID, "sess-b")
	if _, err := regB.get(env.ProposalID); err == nil {
		t.Error("another session rehydrated the proposal")
	}
	regA, _ := LoadProposalRegistry(repo, newMgr, rootID, "sess-a")
	if _, err := regA.get(env.ProposalID); err != nil {
		t.Errorf("owning session did not rehydrate the proposal: %v", err)
	}
}

func TestRetained_WorktreeProposalSurvivesRestart(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	transcript := []types.Message{types.SystemMsg("dev sys"), types.UserMsg("do the work")}
	var calls int
	var gotSeed []types.Message

	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			calls++
			if calls == 1 {
				writeTestFile(t, params.Workdir, "work.txt", "work\n")
				jobs.CaptureSubAgentTranscript(ctx, transcript)
				return "done", nil
			}
			gotSeed = params.SeedMessages
			writeTestFile(t, params.Workdir, "more.txt", "more\n")
			return "continued", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-durable",
		Proposals:    NewProposalRegistry(),
	}

	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" {
		t.Fatalf("dispatch status = %q (%s)", env.Status, env.Error)
	}
	live, err := rt.Proposals.get(env.ProposalID)
	if err != nil {
		t.Fatalf("live proposal: %v", err)
	}
	wantDiff := live.reviewDiff()
	if wantDiff == "" {
		t.Fatalf("sealed proposal has no diff to persist")
	}

	// Simulate a restart: rebuild the scope tree and sealed outputs from the
	// durable trace, then rehydrate the proposal registry from that manager.
	newMgr, _, rerr := recovery.Recover(context.Background(), fx.store, repo, true, nil)
	if rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	reg, warnings := LoadProposalRegistry(repo, newMgr, rootID, "sess-durable")
	p, err := reg.get(env.ProposalID)
	if err != nil {
		t.Fatalf("recovered proposal %s missing: %v (warnings: %v)", env.ProposalID, err, warnings)
	}
	if !reflect.DeepEqual(p.transcript, transcript) {
		t.Errorf("recovered transcript = %+v, want %+v", p.transcript, transcript)
	}
	if p.result != "done" {
		t.Errorf("recovered result = %q, want %q", p.result, "done")
	}
	if p.params.Role != "developer" {
		t.Errorf("recovered params role = %q, want developer", p.params.Role)
	}
	// The sealed output retains the diff; recovery must restore it so a
	// recovered proposal still shows and releases its change.
	if p.reviewDiff() != wantDiff {
		t.Errorf("recovered diff = %q, want %q", p.reviewDiff(), wantDiff)
	}

	// The recovered proposal is continuable: the continuation seeds from the
	// recovered workspace and transcript.
	newRT := RetainedRuntime{
		Runner:       rt.Runner,
		RepoPath:     repo,
		ScopeManager: newMgr,
		RootScopeID:  rootID,
		WorktreeRoot: rt.WorktreeRoot,
		SessionID:    "sess-durable",
		Proposals:    reg,
	}
	res2, err := ContinueRetainedRun(context.Background(), newRT, env.ProposalID, 2, "", false)
	if err != nil {
		t.Fatalf("continue after restart: %v", err)
	}
	if env2 := decodeProposalEnvelope(t, res2); env2.Status != "proposal" {
		t.Fatalf("continuation status = %q (%s)", env2.Status, env2.Error)
	}
	if !reflect.DeepEqual(gotSeed, transcript) {
		t.Errorf("continuation seed after restart = %+v, want %+v", gotSeed, transcript)
	}
}

// TestRetained_WorktreeProposalCitesPlanDigest pins the worktree lane's
// declaration citation. The dispatch compiles the same declaration the container
// lane compiles (see worktreeAuthorityForDispatch) and the digest rides in the
// only durable seal record this lane has — the sealed output's metadata — so the
// live proposal carries it into settlement and a proposal rehydrated after a
// restart still cites the declaration its world enforced instead of
// re-deriving one from config that may have moved.
func TestRetained_WorktreeProposalCitesPlanDigest(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	transcript := []types.Message{types.UserMsg("do the work")}
	var calls int

	runner := func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
		calls++
		writeTestFile(t, params.Workdir, "work.txt", "work\n")
		jobs.CaptureSubAgentTranscript(ctx, transcript)
		return "done", nil
	}
	rt := RetainedRuntime{
		Runner:       runner,
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-plan-digest",
		Proposals:    NewProposalRegistry(),
		Network:      "allow_all",
	}

	want, err := resolveWorktreePlanDigest(rt, "developer", nil)
	if err != nil {
		t.Fatalf("resolveWorktreePlanDigest: %v", err)
	}
	if len(want) != 64 || strings.Trim(want, "0123456789abcdef") != "" {
		t.Fatalf("worktree plan digest = %q, want 64 hex characters", want)
	}

	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" {
		t.Fatalf("dispatch status = %q (%s)", env.Status, env.Error)
	}
	live, err := rt.Proposals.get(env.ProposalID)
	if err != nil {
		t.Fatalf("live proposal: %v", err)
	}
	if live.authorityPlanDigest != want {
		t.Errorf("live proposal cites %q, want the compiled declaration %q", live.authorityPlanDigest, want)
	}

	// The seal's metadata is this lane's durable record, so the digest must ride
	// in it — that is what a later process reads.
	meta, _, merr := proposalMetaFromOutput(live.output)
	if merr != nil {
		t.Fatalf("sealed proposal metadata: %v", merr)
	}
	if meta.AuthorityPlanDigest != want {
		t.Errorf("sealed metadata cites %q, want %q", meta.AuthorityPlanDigest, want)
	}

	// A continuation is a world of its own: it seals its own citation.
	contRes, err := ContinueRetainedRun(context.Background(), rt, env.ProposalID, 2, "", false)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	contEnv := decodeProposalEnvelope(t, contRes)
	if contEnv.Status != "proposal" {
		t.Fatalf("continuation status = %q (%s)", contEnv.Status, contEnv.Error)
	}
	cont, err := rt.Proposals.get(contEnv.ProposalID)
	if err != nil {
		t.Fatalf("continuation proposal: %v", err)
	}
	if cont.authorityPlanDigest != want {
		t.Errorf("continuation cites %q, want %q", cont.authorityPlanDigest, want)
	}
	if calls < 2 {
		t.Fatalf("continuation did not run the body (calls = %d)", calls)
	}

	// A restart re-reads the sealed metadata: the recovered proposal must cite
	// the same declaration it was sealed with.
	newMgr, _, rerr := recovery.Recover(context.Background(), fx.store, repo, true, nil)
	if rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	reg, warnings := LoadProposalRegistry(repo, newMgr, rootID, "sess-plan-digest")
	for _, id := range []string{env.ProposalID, contEnv.ProposalID} {
		p, err := reg.get(id)
		if err != nil {
			t.Fatalf("recovered proposal %s missing: %v (warnings: %v)", id, err, warnings)
		}
		if p.authorityPlanDigest != want {
			t.Errorf("rehydrated proposal %s cites %q, want the sealed %q", id, p.authorityPlanDigest, want)
		}
	}
}
