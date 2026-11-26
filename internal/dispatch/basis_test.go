package dispatch

// basis_test.go covers yaah-witj increment A: the fork-option pairing that makes
// a seed without a matching baseline inexpressible, the durable basis record the
// seal persists, the basis fields on the result envelopes, and the guard for a
// basis that does not name the worktree repository's HEAD.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
)

func testBasisState() kerneltrace.WorkspaceState {
	return kerneltrace.WorkspaceState{
		Backend:  "git",
		Revision: "1111111111111111111111111111111111111111",
		Data: map[string]any{
			"head_sha": "1111111111111111111111111111111111111111",
			"tree_sha": "2222222222222222222222222222222222222222",
		},
	}
}

// TestForkOptionsFor_PairsSeedAndBaseline pins the A1 invariant: a fork's
// contents and the state its delta is measured against are set from one place,
// a seed can never arrive without a usable baseline, and a zero pair still means
// "capture the parent's workspace at fork time".
func TestForkOptionsFor_PairsSeedAndBaseline(t *testing.T) {
	state := testBasisState()

	t.Run("one captured state is both seed and baseline", func(t *testing.T) {
		seed := freshForkSeed(state)
		if !reflect.DeepEqual(seed.seed, seed.baseline) {
			t.Fatalf("fresh seed pair = %+v / %+v, want the same state", seed.seed, seed.baseline)
		}
		opts := seed.forkOptionsFor()
		if !reflect.DeepEqual(opts.Seed, opts.Baseline) {
			t.Fatalf("fork options seed %+v != baseline %+v", opts.Seed, opts.Baseline)
		}
		if !reflect.DeepEqual(opts.Seed, state) {
			t.Fatalf("fork options seed = %+v, want the captured state", opts.Seed)
		}
	})

	t.Run("zero state still captures the parent at fork time", func(t *testing.T) {
		opts := freshForkSeed(kerneltrace.WorkspaceState{}).forkOptionsFor()
		if opts.Seed.Backend != "" || opts.Baseline.Backend != "" {
			t.Fatalf("zero pair produced seed %+v / baseline %+v, want both zero so the runtime captures the parent",
				opts.Seed, opts.Baseline)
		}
	})

	t.Run("a seed with no recorded baseline falls back to the seed", func(t *testing.T) {
		opts := threadSeed{seed: state}.forkOptionsFor()
		if !reflect.DeepEqual(opts.Baseline, state) {
			t.Fatalf("baseline = %+v, want the seed %+v (incremental, never host-time)", opts.Baseline, state)
		}
	})

	t.Run("a recorded baseline is preserved verbatim", func(t *testing.T) {
		recorded := kerneltrace.WorkspaceState{Backend: "git", Revision: "3333333333333333333333333333333333333333"}
		opts := threadSeed{seed: state, baseline: recorded}.forkOptionsFor()
		if !reflect.DeepEqual(opts.Baseline, recorded) {
			t.Fatalf("baseline = %+v, want the recorded %+v (cumulative delta)", opts.Baseline, recorded)
		}
		if !reflect.DeepEqual(opts.Seed, state) {
			t.Fatalf("seed = %+v, want the source workspace", opts.Seed)
		}
	})

	// The failed-seal arm passes no ForkOptions: ForkFromFailedSeal builds them
	// from the handle, so the handle IS the pair. A handle carrying a workspace
	// and no baseline must fork against the workspace, never with a zero baseline
	// (which the runtime reads as "capture the parent at fork time").
	t.Run("a failed-seal artifact forks against its resolved pair", func(t *testing.T) {
		h := &goatherd.FailedSeal{ArtifactID: "art:no-baseline", Workspace: state}
		seed := threadSeed{failedSeal: h, seed: h.Workspace, baseline: h.Baseline}
		handle := seed.failedSealForkHandle()
		if handle == nil {
			t.Fatal("failed-seal seed produced no handle")
		}
		if handle.Baseline.Backend == "" {
			t.Fatalf("handle baseline = %+v, want the artifact workspace %+v: a zero baseline makes the runtime capture the parent at fork time", handle.Baseline, state)
		}
		if !reflect.DeepEqual(handle.Baseline, state) || !reflect.DeepEqual(handle.Workspace, state) {
			t.Fatalf("handle pair = seed %+v / baseline %+v, want both %+v", handle.Workspace, handle.Baseline, state)
		}
		if h.Baseline.Backend != "" {
			t.Fatal("the caller's handle was mutated; the artifact's own record must be left as recorded")
		}
	})

	t.Run("a failed-seal artifact's recorded baseline wins over its workspace", func(t *testing.T) {
		recorded := kerneltrace.WorkspaceState{Backend: "git", Revision: "4444444444444444444444444444444444444444"}
		h := &goatherd.FailedSeal{ArtifactID: "art:baseline", Workspace: state, Baseline: recorded}
		handle := threadSeed{failedSeal: h, seed: h.Workspace, baseline: h.Baseline}.failedSealForkHandle()
		if !reflect.DeepEqual(handle.Baseline, recorded) {
			t.Fatalf("handle baseline = %+v, want the recorded %+v", handle.Baseline, recorded)
		}
		if !reflect.DeepEqual(handle.Workspace, state) {
			t.Fatalf("handle seed = %+v, want the artifact workspace", handle.Workspace)
		}
	})
}

// TestForkOptionsHasOneConstructionSite is the structural half of A1: it fails
// if any file but fork_options.go builds a goatherd.ForkOptions, which is how a
// seed-without-baseline pair would come back.
func TestForkOptionsHasOneConstructionSite(t *testing.T) {
	const marker = "ForkOptions{"
	const home = "fork_options.go"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	foundInHome := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if !bytes.Contains(data, []byte(marker)) {
			continue
		}
		if name != home {
			t.Errorf("%s builds a goatherd.ForkOptions; route it through threadSeed.forkOptionsFor in %s", name, home)
			continue
		}
		foundInHome = true
	}
	if !foundInHome {
		t.Fatalf("%s no longer builds a goatherd.ForkOptions; the scan is stale, not green", home)
	}
}

// TestProposalEnvelopeBasisFields pins A3: the envelope and the list summary
// name the basis, and omit every basis field when none was recorded.
func TestProposalEnvelopeBasisFields(t *testing.T) {
	state := testBasisState()

	t.Run("envelope carries the basis", func(t *testing.T) {
		env := proposalEnvelope{Status: "proposal"}
		env.setBasis(basisRecord(state))
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, want := range []string{
			`"basis_head":"` + state.Revision + `"`,
			`"basis_tree":"2222222222222222222222222222222222222222"`,
			`"basis_backend":"git"`,
		} {
			if !strings.Contains(string(data), want) {
				t.Errorf("envelope %s does not carry %s", data, want)
			}
		}
	})

	t.Run("absent basis omits every field", func(t *testing.T) {
		env := proposalEnvelope{Status: "proposal"}
		env.setBasis(nil)
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), "basis_") {
			t.Errorf("envelope with no recorded basis emitted one: %s", data)
		}
	})

	t.Run("list summary carries the basis", func(t *testing.T) {
		summary := proposalSummary{ProposalID: "p1"}
		summary.setBasis(basisRecord(state))
		data, err := json.Marshal(summary)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), `"basis_head":"`+state.Revision+`"`) {
			t.Errorf("summary %s does not carry the basis head", data)
		}
		bare, err := json.Marshal(proposalSummary{ProposalID: "p1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(bare), "basis_") {
			t.Errorf("summary with no recorded basis emitted one: %s", bare)
		}
	})

	t.Run("zero state records nothing", func(t *testing.T) {
		if rec := basisRecord(kerneltrace.WorkspaceState{}); rec != nil {
			t.Fatalf("zero state produced %+v, want no record", rec)
		}
	})
}

// TestPersistGitDelta_PersistsBasis pins A2 on the container lane: the basis is
// written into meta.json beside the manifest and read back by rehydration, so a
// proposal rehydrated in a later process still names what it started from.
func TestPersistGitDelta_PersistsBasis(t *testing.T) {
	f := newDeltaFixture(t)
	p := f.add(t, "proposal:basis", rehydrateDelta())

	var meta proposalMeta
	if err := readJSON(p.artifactDir+"/"+proposalMetaFile, &meta); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if meta.Basis == nil {
		t.Fatal("meta.json carries no basis")
	}
	if meta.Basis.Backend != f.basisState.Backend || meta.Basis.Head != f.basisState.Revision {
		t.Errorf("meta basis = %+v, want backend %q head %q", meta.Basis, f.basisState.Backend, f.basisState.Revision)
	}

	reg, warnings := LoadProposalRegistry(f.repo, f.fx.mgr, f.rootID, f.sessionID)
	if len(warnings) != 0 || len(reg.all()) != 1 {
		t.Fatalf("proposals = %d, warnings %v; want one", len(reg.all()), warnings)
	}
	rec := reg.all()[0].dispatchBasisRecord()
	if rec == nil || rec.Head != f.basisState.Revision {
		t.Errorf("rehydrated basis = %+v, want head %q", rec, f.basisState.Revision)
	}
}

// TestRetained_BasisSurvivesSealAndRestart pins A2 on the worktree lane: the
// seal persists the resolved basis, the envelope reports it, and a proposal
// rehydrated after a restart still names the commit its work started from.
func TestRetained_BasisSurvivesSealAndRestart(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	head, herr := gitHeadSHA(context.Background(), repo)
	if herr != nil || head == "" {
		t.Fatalf("read repo HEAD: %q %v", head, herr)
	}
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			jobs.CaptureSubAgentTranscript(ctx, []types.Message{types.UserMsg("go")})
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-basis",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" {
		t.Fatalf("status = %q (%s)", env.Status, env.Error)
	}
	if env.BasisHead != head {
		t.Errorf("envelope basis_head = %q, want the repo HEAD %q", env.BasisHead, head)
	}
	if env.BasisBackend != "git" {
		t.Errorf("envelope basis_backend = %q, want git", env.BasisBackend)
	}
	if env.BasisTree == "" {
		t.Error("envelope carries no basis_tree")
	}

	p, err := rt.Proposals.get(env.ProposalID)
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	meta, _, merr := proposalMetaFromOutput(p.output)
	if merr != nil {
		t.Fatalf("read sealed metadata: %v", merr)
	}
	if meta.Basis == nil || meta.Basis.Head != head {
		t.Fatalf("sealed metadata basis = %+v, want head %q", meta.Basis, head)
	}

	// A restart rehydrates the proposal from the trace; the basis is still
	// there, which is what a later settlement or continuation reads it for.
	newMgr, _, rerr := recovery.Recover(context.Background(), fx.store, repo, true, nil)
	if rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}
	reg, warnings := LoadProposalRegistry(repo, newMgr, rootID, "sess-basis")
	if len(warnings) != 0 {
		t.Fatalf("rehydrate warnings: %v", warnings)
	}
	recovered, err := reg.get(env.ProposalID)
	if err != nil {
		t.Fatalf("rehydrated proposal missing: %v", err)
	}
	rec := recovered.dispatchBasisRecord()
	if rec == nil || rec.Head != head || rec.Backend != "git" {
		t.Fatalf("rehydrated basis = %+v, want backend git head %q", rec, head)
	}
}

// TestBasisDivergenceWarning pins A4: the worktree repository's HEAD and the
// captured basis are read from different places, so a disagreement is reported
// rather than assumed away.
func TestBasisDivergenceWarning(t *testing.T) {
	repo := newTestGitRepo(t)
	ctx := context.Background()
	head, err := gitHeadSHA(ctx, repo)
	if err != nil || head == "" {
		t.Fatalf("read repo HEAD: %q %v", head, err)
	}
	matching := kerneltrace.WorkspaceState{
		Backend:  "git",
		Revision: head,
		Data:     map[string]any{"head_sha": head, "tree_sha": "t"},
	}
	if w, _ := basisDivergence(ctx, repo, matching); w != "" {
		t.Errorf("matching basis warned: %s", w)
	}
	divergent := kerneltrace.WorkspaceState{
		Backend:  "git",
		Revision: strings.Repeat("ab", 20),
		Data:     map[string]any{"head_sha": strings.Repeat("ab", 20)},
	}
	w, fact := basisDivergence(ctx, repo, divergent)
	if !strings.Contains(w, "dispatch basis") || !strings.Contains(w, head) {
		t.Errorf("divergent basis warning = %q, want it to name both commits", w)
	}
	// The durable half names the same two commits, so the fact cannot drift from
	// the warning it was recorded with.
	if fact["basis_head"] != strings.Repeat("ab", 20) || fact["repo_head"] != head {
		t.Errorf("divergence fact = %v, want basis_head %q and repo_head %q", fact, strings.Repeat("ab", 20), head)
	}
	if w, fact := basisDivergence(ctx, "", divergent); w != "" || fact != nil {
		t.Errorf("no repo path warned: %q / %v", w, fact)
	}
	if w, fact := basisDivergence(ctx, repo, kerneltrace.WorkspaceState{Backend: "croft", Revision: "snap"}); w != "" || fact != nil {
		t.Errorf("non-git basis warned: %q / %v", w, fact)
	}
}

// TestAddDispatchWarnings_ScalarRepeatsFirst pins the AddWarning contract at the
// one place a dispatch stacks two caveats: a degraded world and a basis
// divergence. The scalar `warning` must repeat warnings[0] (proposal.md says so)
// rather than being assigned behind the list's back, which is what hid the
// basis caveat from every reader of the scalar alone (yaah-witj review).
func TestAddDispatchWarnings_ScalarRepeatsFirst(t *testing.T) {
	const basisWarning = "dispatch basis abc differs from the worktree repository's HEAD def"
	env := proposalEnvelope{Status: "proposal"}
	addDispatchWarnings(&env, subagent.WorldResolution{Degraded: true, Reason: "no container"}, []string{basisWarning})

	if len(env.Warnings) != 2 {
		t.Fatalf("warnings = %q, want the degraded world and the basis caveat", env.Warnings)
	}
	if env.Warning != env.Warnings[0] {
		t.Fatalf("warning = %q, want warnings[0] %q", env.Warning, env.Warnings[0])
	}
	if !strings.Contains(env.Warnings[0], "degraded") {
		t.Errorf("warnings[0] = %q, want the degraded world first (the message the scalar always carried)", env.Warnings[0])
	}
	if env.Warnings[1] != basisWarning {
		t.Errorf("warnings[1] = %q, want the basis caveat %q", env.Warnings[1], basisWarning)
	}

	// A basis warning on its own still reaches the scalar.
	only := proposalEnvelope{Status: "proposal"}
	addDispatchWarnings(&only, subagent.WorldResolution{}, []string{basisWarning})
	if only.Warning != basisWarning || len(only.Warnings) != 1 {
		t.Errorf("warning = %q / warnings %q, want the basis caveat in both", only.Warning, only.Warnings)
	}
}

// TestDispatchBasisRecord_FallsBackToTheSealRecord pins the reader the seal's
// basis record was added for: the live state answers first, and the persisted
// record answers when no state is readable (yaah-witj increment A).
func TestDispatchBasisRecord_FallsBackToTheSealRecord(t *testing.T) {
	rec := &proposalBasisRecord{Backend: "git", Head: "aaaa", Tree: "bbbb"}
	p := &retainedProposal{sealBasisRecord: rec}
	if got := p.dispatchBasisRecord(); got == nil || got.Head != "aaaa" || got.Tree != "bbbb" {
		t.Fatalf("record = %+v, want the seal's own record when no state is readable", got)
	}
	if rec := (&retainedProposal{}).dispatchBasisRecord(); rec != nil {
		t.Fatalf("record = %+v, want none when neither state nor seal record exists", rec)
	}

	state := testBasisState()
	p.basisState = state
	if got := p.dispatchBasisRecord(); got == nil || got.Head != state.Revision {
		t.Fatalf("record = %+v, want the live state head %q to win", got, state.Revision)
	}
}

// TestFailedSealBasis_ResolvesLikeTheFork pins the reporting half of the
// failed-seal resolution: the basis a failed-seal proposal reports is the one its
// fork (and so its continuation) is measured against, and a handle with neither a
// baseline nor a workspace reports no basis at all rather than an empty one.
func TestFailedSealBasis_ResolvesLikeTheFork(t *testing.T) {
	state := testBasisState()
	workspaceOnly := &retainedProposal{
		id:         "art:workspace-only",
		status:     "seal_failed",
		failedSeal: &goatherd.FailedSeal{ArtifactID: "art:workspace-only", Workspace: state},
	}
	if rec := workspaceOnly.dispatchBasisRecord(); rec == nil || rec.Head != state.Revision {
		t.Fatalf("record = %+v, want the artifact workspace head %q (the same pair the fork uses)", rec, state.Revision)
	}

	empty := &retainedProposal{
		id:         "art:bare",
		status:     "seal_failed",
		failedSeal: &goatherd.FailedSeal{ArtifactID: "art:bare"},
	}
	if rec := empty.dispatchBasisRecord(); rec != nil {
		t.Fatalf("record = %+v, want none for an artifact with no workspace state", rec)
	}
	env := proposalEnvelope{Status: "settled"}
	env.setBasis(empty.dispatchBasisRecord())
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "basis_") {
		t.Errorf("a settlement with no recorded basis emitted one: %s", data)
	}
}

// TestRetained_SettleCarriesBasis pins the pre-consumption basis snapshot on the
// default worktree settle path (yaah-witj increment A review). Each of the four
// verbs consumes the sealed output, and the settlement trace fact is the only
// record of the basis left afterwards — so the fact must name it, and must name
// the same basis the response reported.
//
// Both channels are asserted on purpose. The envelope alone would prove nothing
// about the hoist (setBasis runs before Settle whichever way the basis is read);
// the fact is the half a post-consumption re-read would empty, and before this
// test nothing in this package read a settle fact's basis at all.
func TestRetained_SettleCarriesBasis(t *testing.T) {
	for _, action := range []string{"select", "apply", "release", "discard"} {
		t.Run(action, func(t *testing.T) {
			fx, repo, rootID := newSessionFixtures(t)
			head, herr := gitHeadSHA(context.Background(), repo)
			if herr != nil || head == "" {
				t.Fatalf("read repo HEAD: %q %v", head, herr)
			}
			rt := RetainedRuntime{
				Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
					writeTestFile(t, params.Workdir, "work.txt", "work\n")
					return "done", nil
				},
				RepoPath:     repo,
				ScopeManager: fx.mgr,
				RootScopeID:  rootID,
				WorktreeRoot: t.TempDir(),
				SessionID:    "sess-settle-basis",
				Proposals:    NewProposalRegistry(),
			}
			res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer"}, 10*time.Second, false)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			dispatched := decodeProposalEnvelope(t, res)
			if dispatched.Status != "proposal" {
				t.Fatalf("dispatch status = %q (%s)", dispatched.Status, dispatched.Error)
			}
			if dispatched.BasisHead != head || dispatched.BasisBackend != "git" || dispatched.BasisTree == "" {
				t.Fatalf("dispatch basis = %+v, want backend git, head %q and a tree", dispatched, head)
			}

			settled, err := rt.Proposals.settleRetainedProposal(context.Background(), dispatched.ProposalID, action)
			if err != nil {
				t.Fatalf("settle %s: %v", action, err)
			}
			env := decodeProposalEnvelope(t, settled)
			if env.Status != "settled" || env.Action != action {
				t.Fatalf("settle envelope = %+v, want settled/%s", env, action)
			}
			if env.BasisHead != dispatched.BasisHead || env.BasisTree != dispatched.BasisTree || env.BasisBackend != dispatched.BasisBackend {
				t.Errorf("%s envelope basis = head %q tree %q backend %q, want the dispatch's head %q tree %q backend %q",
					action, env.BasisHead, env.BasisTree, env.BasisBackend,
					dispatched.BasisHead, dispatched.BasisTree, dispatched.BasisBackend)
			}

			// The durable half: the fact is what a post-settlement reader has
			// once the output and the artifact are gone.
			payload := settleFactPayloadFor(t, fx, rootID, dispatched.ProposalID, action)
			for key, want := range map[string]string{
				"basis_backend": dispatched.BasisBackend,
				"basis_head":    dispatched.BasisHead,
				"basis_tree":    dispatched.BasisTree,
			} {
				got, _ := payload[key].(string)
				if got == "" {
					t.Errorf("%s settle fact carries no %s: %v", action, key, payload)
					continue
				}
				if got != want {
					t.Errorf("%s settle fact %s = %q, want %q (the basis the response named)", action, key, got, want)
				}
			}
		})
	}
}

// settleFactPayloadFor returns the settle fact recorded for one proposal and
// action on the session root's owner path.
func settleFactPayloadFor(t *testing.T, fx *sessionFixtures, rootID, proposalID, action string) map[string]any {
	t.Helper()
	parent, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	for _, fact := range findRetainedFacts(t, fx.store, parent.OwnerID(), SchemaProposalSettled) {
		data, merr := json.Marshal(fact.Body.Payload)
		if merr != nil {
			continue
		}
		var probe struct {
			ProposalID string `json:"proposal_id"`
			Action     string `json:"action"`
		}
		if json.Unmarshal(data, &probe) != nil {
			continue
		}
		if probe.ProposalID == proposalID && probe.Action == action {
			return fact.Body.Payload
		}
	}
	t.Fatalf("no settle fact recorded for %s %s", proposalID, action)
	return nil
}

// TestSettleFactBasisIsReadBeforeTheConsume is the ordering tripwire for the
// hoist in each lane that appends a basis-bearing settle fact: the basis the
// fact carries must come from a snapshot taken BEFORE the sealed output (or the
// container artifact directory) is consumed.
//
// It exists because the hoist cannot be pinned behaviourally against the
// runtime this package depends on. goatherd-runtime's RetainedOutput.Baseline()
// is a field read that Settle does not clear (Settle's only mutation is
// `out.state = terminal`), so a post-consumption re-read still answers the same
// record and every assertion in TestRetained_SettleCarriesBasis above passes
// either way — verified by mutation, not assumed. What makes the re-read wrong
// is that it makes the one post-settlement record depend on an upstream
// implementation detail this package does not own, so the invariant is pinned
// where it is actually stated: the order of the statements.
//
// What it does NOT prove: it is a lexical, single-file check on this package's
// source, not a semantic one. It cannot tell that a snapshot variable is
// genuinely untouched between the hoist and the fact, and it cannot see a
// re-derivation hidden behind a helper function. It will fail loudly if the
// fact-bearing identifier is renamed — that is deliberate: update the scan
// rather than loosen it.
func TestSettleFactBasisIsReadBeforeTheConsume(t *testing.T) {
	cases := []struct {
		file string
		// consume is the statement after which the record the fact needs is no
		// longer trustworthy.
		consume string
		what    string
	}{
		{"retained.go", "p.output.Settle(", "the sealed output is consumed by Settle"},
		{"proposal_merge.go", "p.output.Settle(", "the sealed output is consumed by Settle"},
		{"git_delta.go", "os.RemoveAll(p.artifactDir)", "the artifact directory is consumed at the end of the settle"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			lines := readPackageSourceLines(t, tc.file)
			factIdx := -1
			for i, line := range lines {
				if !strings.Contains(line, ".factFields()") {
					continue
				}
				if factIdx >= 0 {
					t.Fatalf("%s builds settle-fact basis fields twice; the scan assumes one per lane", tc.file)
				}
				factIdx = i
			}
			if factIdx < 0 {
				t.Fatalf("%s no longer builds settle-fact basis fields; the scan is stale, not green", tc.file)
			}
			if strings.Contains(lines[factIdx], "dispatchBasisRecord(") {
				t.Fatalf("%s:%d rebuilds the basis from the proposal at the fact site: %s\n%s, so a post-consumption re-read can empty the fact; capture the record before %s and pass it down",
					tc.file, factIdx+1, strings.TrimSpace(lines[factIdx]), tc.what, tc.consume)
			}
			receiver := factLoopReceiver(lines[factIdx])
			if receiver == "" || strings.ContainsAny(receiver, "()[]{}.") {
				t.Fatalf("%s:%d builds the basis fields from %q, not a captured snapshot variable: %s",
					tc.file, factIdx+1, receiver, strings.TrimSpace(lines[factIdx]))
			}

			assignIdx, assigns := -1, 0
			for i, line := range lines {
				if !strings.Contains(line, "dispatchBasisRecord()") {
					continue
				}
				if !strings.Contains(line, receiver+" :=") && !strings.Contains(line, receiver+" =") {
					continue
				}
				assignIdx = i
				assigns++
			}
			if assigns != 1 {
				t.Fatalf("%s assigns %s from dispatchBasisRecord() %d times; want exactly one hoisted snapshot", tc.file, receiver, assigns)
			}
			consumeIdx := -1
			for i, line := range lines {
				if strings.Contains(line, tc.consume) {
					consumeIdx = i
					break
				}
			}
			if consumeIdx < 0 {
				t.Fatalf("%s no longer contains %q; the scan is stale, not green", tc.file, tc.consume)
			}
			if assignIdx >= consumeIdx {
				t.Fatalf("%s:%d reads the basis at or after %q (%s:%d, %s); the snapshot must be taken first",
					tc.file, assignIdx+1, tc.consume, tc.file, consumeIdx+1, tc.what)
			}
			if assignIdx >= factIdx {
				t.Fatalf("%s:%d reads the basis below the fact that uses it (%s:%d)", tc.file, assignIdx+1, tc.file, factIdx+1)
			}
		})
	}
}

// factLoopReceiver returns the identifier a `range <id>.factFields()` loop ranges
// over, or "" when the line has no such shape.
func factLoopReceiver(line string) string {
	cut := strings.Index(line, ".factFields()")
	if cut < 0 {
		return ""
	}
	prefix := strings.TrimSpace(line[:cut])
	if i := strings.LastIndex(prefix, "range "); i >= 0 {
		prefix = prefix[i+len("range "):]
	}
	return strings.TrimSpace(prefix)
}

// readPackageSourceLines returns one of this package's non-test sources as lines.
func readPackageSourceLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.Split(string(data), "\n")
}
