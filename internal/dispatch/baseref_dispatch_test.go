package dispatch

// baseref_dispatch_test.go covers the base_ref dispatch wiring (yaah-witj
// increment B): the refusal on a resolved container world, the fact that the
// child's world really starts at the named commit, and the settlement
// consequence — a base_ref proposal settles with apply, never select.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/tools/substrate"
)

// baseRefSideBranch creates a `side` branch at the current HEAD and then moves
// main on, so a dispatch seeded at `side` cannot be confused with one seeded at
// HEAD. It returns the side commit.
func baseRefSideBranch(t *testing.T, repo string) string {
	t.Helper()
	side := revParseTest(t, repo, "HEAD")
	mustRunGit(t, repo, "branch", "side", side)
	writeTestFile(t, repo, "moved-on.txt", "moved on\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "main moves on")
	return side
}

// TestRetained_BaseRefRefusedOnAResolvedContainerWorld pins the lane rule for
// the RESOLVED world: a container world is seeded from a session snapshot or a
// host-repo copy-in, not from a ref, so a dispatch that ends up there is
// refused — and it is refused before anything is provisioned.
func TestRetained_BaseRefRefusedOnAResolvedContainerWorld(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			t.Error("the sub-agent ran: a base_ref dispatch must be refused before the world exists")
			return "", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-container",
		Proposals:    NewProposalRegistry(),
		// Wired dispatch worlds make an `auto` proposal resolve to the
		// container; the refusal keys on that resolution, not on the request.
		ContainerWorlds: true,
		DispatchWorlds:  &DispatchWorlds{},
	}
	_, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: "HEAD"}, 10*time.Second, false)
	if err == nil {
		t.Fatal("base_ref on a resolved container world = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "worktree-lane only") {
		t.Fatalf("error = %q, want a worktree-lane refusal naming the world", err)
	}
}

// TestRetained_BaseRefReprovisionsAtTheRef is the seeding proof: the worktree
// is created at the repository HEAD and then seeded, so the ref's commit is
// what the child actually holds — including the ABSENCE of a file the parent
// committed after it.
//
// The assertion happens INSIDE the run: a report world is torn down at teardown
// and a container world ends at seal, so the worktree is only readable while
// the body is running (or on an open proposal-lane worktree).
func TestRetained_BaseRefReprovisionsAtTheRef(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	var seenHead string
	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			seenHead = mustGitIn(t, params.Workdir, "rev-parse", "HEAD")
			if _, err := os.Stat(filepath.Join(params.Workdir, "moved-on.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the child's world holds moved-on.txt (err=%v); it must start at the ref, not at HEAD", err)
			}
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-seed",
		Proposals:    NewProposalRegistry(),
	}
	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" {
		t.Fatalf("status = %q (%s)", env.Status, env.Error)
	}
	if seenHead != side {
		t.Fatalf("child worktree HEAD = %q, want the base_ref commit %q", seenHead, side)
	}
	// The recorded basis is the RESOLVED state, not the request: it names the
	// commit the ref resolved to, which is what settlement measures against.
	if env.BasisHead != side {
		t.Fatalf("envelope basis head = %q, want %q", env.BasisHead, side)
	}
	if env.BasisBackend != "git" || env.BasisTree == "" {
		t.Fatalf("envelope basis = %+v, want backend git and a tree", env)
	}
	if want := revParseTest(t, repo, side+"^{tree}"); env.BasisTree != want {
		t.Fatalf("envelope basis tree = %q, want %q", env.BasisTree, want)
	}
	// The bead's second acceptance criterion: the proposal's files/diff reflect
	// the TARGET branch, i.e. the delta is measured against the named ref and
	// not against the parent's HEAD. The distinction is visible exactly here —
	// measured against HEAD the delta would contain a deletion of moved-on.txt
	// (present at HEAD, absent at the seed), so the sealed work would look like
	// a revert of the parent's own commit.
	if len(env.Files) != 1 || env.Files[0] != "work.txt" {
		t.Fatalf("proposal files = %v, want only the child's own work.txt", env.Files)
	}
	if !strings.Contains(env.Diff, "work.txt") {
		t.Fatalf("proposal diff does not mention work.txt:\n%s", env.Diff)
	}
	if strings.Contains(env.Diff, "moved-on.txt") {
		t.Fatalf("proposal diff mentions moved-on.txt: the delta is being measured against the parent's HEAD rather than at the named ref:\n%s", env.Diff)
	}
	// A named ref is a DELIBERATE basis/HEAD divergence, so the increment-A
	// caveat must not fire and call it a surprise.
	for _, w := range env.Warnings {
		if strings.Contains(w, "differs from the worktree repository's HEAD") {
			t.Fatalf("unexpected basis-divergence warning on a base_ref dispatch: %q", w)
		}
	}
}

// TestRetained_BaseRefSettlesViaApplyWhenParentMoved is the bead's acceptance
// criterion, and it is an APPLY test by design: select compares digests that
// include the HEAD sha, so a dispatch seeded at a non-HEAD basis always refuses
// select. That is fail-closed behaviour the owner chose to keep, not a defect —
// apply is the base_ref settlement path.
func TestRetained_BaseRefSettlesViaApplyWhenParentMoved(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)

	rt := RetainedRuntime{
		Runner: func(ctx context.Context, prompt string, params SubAgentParams) (string, error) {
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-apply",
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
	if dispatched.BasisHead != side {
		t.Fatalf("basis head = %q, want the ref's commit %q", dispatched.BasisHead, side)
	}

	// The parent moves on AFTER the dispatch, on a path the proposal does not
	// touch — the situation base_ref exists for.
	writeTestFile(t, repo, "parent-later.txt", "later\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "parent moves after the dispatch")

	// select fails closed: the basis is not the parent's HEAD, by design.
	if _, err := rt.Proposals.settleRetainedProposal(context.Background(), dispatched.ProposalID, "select"); err == nil {
		t.Fatal("select on a base_ref proposal succeeded; it must fail closed against a non-HEAD basis")
	} else if !strings.Contains(err.Error(), "use SettleApply") {
		t.Fatalf("select error = %q, want the fail-closed basis refusal", err)
	}

	settled, err := rt.Proposals.settleRetainedProposal(context.Background(), dispatched.ProposalID, "apply")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	env := decodeProposalEnvelope(t, settled)
	if env.Status != "settled" || env.Action != "apply" {
		t.Fatalf("settle envelope = %+v, want settled/apply", env)
	}
	if env.Strategy != proposalApplyPathDisjoint {
		t.Fatalf("strategy = %q, want %q", env.Strategy, proposalApplyPathDisjoint)
	}
	if env.BasisHead != dispatched.BasisHead || env.BasisTree != dispatched.BasisTree {
		t.Fatalf("settle basis = head %q tree %q, want the dispatch's %q / %q", env.BasisHead, env.BasisTree, dispatched.BasisHead, dispatched.BasisTree)
	}
	content, rerr := os.ReadFile(filepath.Join(repo, "work.txt"))
	if rerr != nil {
		t.Fatalf("apply did not land the proposal's file: %v", rerr)
	}
	if string(content) != "work\n" {
		t.Fatalf("work.txt = %q, want the proposal's content", content)
	}
	if _, serr := os.Stat(filepath.Join(repo, "parent-later.txt")); serr != nil {
		t.Fatalf("the parent's own later commit was disturbed: %v", serr)
	}
}

// mustGitIn runs one git command in dir and returns its trimmed stdout.
func mustGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}

// TestRetained_BaseRefRefusesTheSessionContainerFallback pins both report-lane
// fallbacks that would otherwise run a caller-named-basis dispatch on the
// SESSION's tree: the world-unavailable step-down (which fires before the
// container gate) and the worktree-provisioning step-down (which fires after
// it). seed_from cannot reach either path because a report is refused earlier,
// so without these gates base_ref would be a NEW silent-drop class — the class
// increment A exists to prevent.
func TestRetained_BaseRefRefusesTheSessionContainerFallback(t *testing.T) {
	t.Run("world unavailable", func(t *testing.T) {
		fx, repo, rootID := newSessionFixtures(t)
		baseRefSideBranch(t, repo)
		ran := false
		rt := RetainedRuntime{
			Runner: func(context.Context, string, SubAgentParams) (string, error) {
				ran = true
				return "session fallback report", nil
			},
			RepoPath:     repo,
			ScopeManager: fx.mgr,
			RootScopeID:  rootID,
			WorktreeRoot: t.TempDir(),
			SessionID:    "sess-baseref-fallback-world",
			Proposals:    NewProposalRegistry(),
			World:        "auto",
			Containment:  "auto",
			Network:      "allow_all",
			// A container session that offers no sibling world: neither world of
			// the ladder is legal, so resolution fails and the report lane would
			// step down into the session container.
			DispatchWorlds: &DispatchWorlds{SessionSandbox: &substrate.SessionSandbox{}},
		}
		_, err := StartRetainedRun(context.Background(), rt, "inspect", SubAgentParams{Role: "reviewer", BaseRef: "side"}, 10*time.Second, true)
		if err == nil {
			t.Fatal("a base_ref report with no seedable world = nil error, want a refusal")
		}
		if strings.Contains(err.Error(), "cannot preserve") {
			t.Fatalf("fixture shape is wrong: the session-container fallback was never allowed, so this test proves nothing: %v", err)
		}
		if ran {
			t.Fatal("the body ran in the session container: a caller-named basis must never degrade to a different tree")
		}
		for _, want := range []string{"base_ref", "session-container fallback"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("worktree provisioning failed", func(t *testing.T) {
		fx, repo, rootID := newSessionFixtures(t)
		baseRefSideBranch(t, repo)
		// A regular file where the worktree root should be: provisioning cannot
		// create the world, so the report lane would step down into the session
		// container.
		broken := filepath.Join(t.TempDir(), "not-a-dir")
		if werr := os.WriteFile(broken, []byte("x"), 0o644); werr != nil {
			t.Fatalf("write fixture file: %v", werr)
		}
		ran := false
		rt := RetainedRuntime{
			Runner: func(context.Context, string, SubAgentParams) (string, error) {
				ran = true
				return "session fallback report", nil
			},
			RepoPath:     repo,
			ScopeManager: fx.mgr,
			RootScopeID:  rootID,
			WorktreeRoot: broken,
			SessionID:    "sess-baseref-fallback-provision",
			Proposals:    NewProposalRegistry(),
		}
		res, err := StartRetainedRun(context.Background(), rt, "inspect", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, true)
		if err != nil {
			t.Fatalf("a provisioning failure is reported in the envelope, not as an error: %v", err)
		}
		if ran {
			t.Fatal("the body ran in the session container: a caller-named basis must never degrade to a different tree")
		}
		env := decodeProposalEnvelope(t, res)
		if env.Status != "error" {
			t.Fatalf("status = %q, want error", env.Status)
		}
		found := false
		for _, w := range env.Warnings {
			if strings.Contains(w, "session-container fallback was declined") {
				found = true
			}
		}
		if !found {
			t.Fatalf("warnings = %v, want the declined fallback named", env.Warnings)
		}
	})
}

// TestRunBlankBaseRefBehavesLikeAbsent pins the one normalization rule: a
// whitespace-only base_ref is ABSENT on every lane, because it is trimmed once
// in Run. Left untrimmed it is judged by two rules in two layers — the shared
// and seed_from gates trim it while the worktree-lane gate does not — so a
// caller that passed `base_ref: "  "` gets either a silent acceptance or a
// refusal naming a blank value.
func TestRunBlankBaseRefBehavesLikeAbsent(t *testing.T) {
	ctx := context.Background()

	t.Run("shared lane", func(t *testing.T) {
		res, err := Run(ctx, DispatchRequest{
			Lane:   LaneShared,
			Params: SubAgentParams{Role: "worker", BaseRef: "   "},
			Prompt: "do the work",
			Runner: func(context.Context, string, SubAgentParams) (string, error) {
				return "the report", nil
			},
		})
		if err != nil {
			t.Fatalf("a blank base_ref on the shared lane = %v, want it treated as absent", err)
		}
		if res.Status != DispatchCompleted || res.Result != "the report" {
			t.Fatalf("result = %+v, want a completed dispatch", res)
		}
	})

	t.Run("proposal lane", func(t *testing.T) {
		rt := newRetainedTestEnv(t, writingRunner(t, "out.txt"))
		res, err := Run(ctx, DispatchRequest{
			Lane:     LaneProposal,
			Params:   SubAgentParams{Role: "worker", BaseRef: "   "},
			Prompt:   "do the work",
			Retained: &rt,
		})
		if err != nil {
			t.Fatalf("a blank base_ref on the proposal lane = %v, want it treated as absent", err)
		}
		if res.Proposal == nil || res.Proposal.Status != "proposal" {
			t.Fatalf("result = %+v, want a sealed proposal", res.Proposal)
		}
		// The basis is the parent's workspace at dispatch, exactly as with no
		// base_ref at all, and no ref is named on the envelope.
		if want := revParseTest(t, rt.RepoPath, "HEAD"); res.Proposal.BasisHead != want {
			t.Fatalf("basis head = %q, want the parent's HEAD %q", res.Proposal.BasisHead, want)
		}
		if res.Proposal.BaseRef != "" {
			t.Fatalf("envelope base_ref = %q, want it absent for a blank request", res.Proposal.BaseRef)
		}
	})

	t.Run("container report lane", func(t *testing.T) {
		fx, repo, rootID := newSessionFixtures(t)
		open, _ := cleanOpener()
		rt := containerReportRuntime(t, fx, repo, rootID, t.TempDir(), func(context.Context, string, SubAgentParams) (string, error) {
			return "container report", nil
		}, "auto", open)
		res, err := Run(ctx, DispatchRequest{
			Lane:     LaneReport,
			Params:   SubAgentParams{Role: "reviewer", BaseRef: "   "},
			Prompt:   "inspect",
			Retained: &rt,
		})
		if err != nil {
			t.Fatalf("a blank base_ref on a container world = %v, want it treated as absent (an untrimmed blank is refused as worktree-lane only)", err)
		}
		if !strings.Contains(res.Result, "container report") {
			t.Fatalf("result = %q, want the report body", res.Result)
		}
	})
}

// TestRetained_BaseRefSeedsWhenAutoDegradesToTheWorktree pins the reason the
// lane refusal keys on the RESOLVED world rather than the requested one: an
// `auto` world legitimately degrades container->worktree, and that dispatch must
// be carried at its ref, not refused.
func TestRetained_BaseRefSeedsWhenAutoDegradesToTheWorktree(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)
	var seenHead string
	rt := RetainedRuntime{
		Runner: func(_ context.Context, _ string, params SubAgentParams) (string, error) {
			seenHead = mustGitIn(t, params.Workdir, "rev-parse", "HEAD")
			writeTestFile(t, params.Workdir, "work.txt", "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-degraded",
		Proposals:    NewProposalRegistry(),
		World:        "auto",
		Containment:  "auto",
		Network:      "allow_all",
		// A build that OFFERS container worlds without a wired dispatch setup:
		// auto resolves to the worktree and records the step-down.
		ContainerWorlds: true,
	}
	res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: "side"}, 10*time.Second, false)
	if err != nil {
		t.Fatalf("a degraded-auto dispatch at a ref must not be refused: %v", err)
	}
	env := decodeProposalEnvelope(t, res)
	if env.Status != "proposal" {
		t.Fatalf("status = %q (%s), want proposal", env.Status, env.Error)
	}
	if seenHead != side {
		t.Fatalf("child worktree HEAD = %q, want the base_ref commit %q", seenHead, side)
	}
	if env.BaseRef != "side" {
		t.Fatalf("envelope base_ref = %q, want the caller's spelling", env.BaseRef)
	}
	degraded := false
	for _, w := range env.Warnings {
		if strings.Contains(w, "dispatch world degraded") {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("warnings = %v, want the recorded degradation", env.Warnings)
	}
}

// TestRetained_BaseRefTwoDispatchesAtTwoRefs is the bead's first acceptance
// criterion: two dispatches on two branches, with no `git checkout` anywhere.
// Both worlds are live at once afterwards, each holding its own ref's commit,
// and the parent repository's checkout is untouched — which is the property a
// single dispatch at one ref cannot show.
func TestRetained_BaseRefTwoDispatchesAtTwoRefs(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	side := baseRefSideBranch(t, repo)
	mainHead := revParseTest(t, repo, "HEAD")

	var heads, workdirs []string
	rt := RetainedRuntime{
		Runner: func(_ context.Context, _ string, params SubAgentParams) (string, error) {
			heads = append(heads, mustGitIn(t, params.Workdir, "rev-parse", "HEAD"))
			workdirs = append(workdirs, params.Workdir)
			writeTestFile(t, params.Workdir, fmt.Sprintf("work-%d.txt", len(heads)), "work\n")
			return "done", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-two-refs",
		Proposals:    NewProposalRegistry(),
	}
	cases := []struct{ ref, wantHead string }{{"side", side}, {"HEAD", mainHead}}
	var ids []string
	for _, tc := range cases {
		res, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", BaseRef: tc.ref}, 10*time.Second, false)
		if err != nil {
			t.Fatalf("dispatch at %q: %v", tc.ref, err)
		}
		env := decodeProposalEnvelope(t, res)
		if env.Status != "proposal" {
			t.Fatalf("dispatch at %q: status = %q (%s)", tc.ref, env.Status, env.Error)
		}
		ids = append(ids, env.ProposalID)
	}

	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("proposal ids = %v, want two distinct proposals", ids)
	}
	for i, tc := range cases {
		if heads[i] != tc.wantHead {
			t.Fatalf("dispatch %d at %q: child HEAD = %q, want %q", i, tc.ref, heads[i], tc.wantHead)
		}
	}
	if workdirs[0] == workdirs[1] {
		t.Fatalf("both dispatches ran in %q; each needs its own world", workdirs[0])
	}
	// No checkout happened: the parent repository is still on its own branch, at
	// its own commit, while both worlds exist.
	if got := revParseTest(t, repo, "HEAD"); got != mainHead {
		t.Fatalf("parent repository HEAD = %q, want %q — a dispatch must not move the parent's checkout", got, mainHead)
	}
	// The two worlds are live at once and neither interfered with the other: the
	// one seeded at `side` predates the commit that moved main on.
	for i, tc := range cases {
		if got := mustGitIn(t, workdirs[i], "rev-parse", "HEAD"); got != tc.wantHead {
			t.Fatalf("world %d HEAD = %q, want %q (worlds must not share a commit)", i, got, tc.wantHead)
		}
	}
	if _, err := os.Stat(filepath.Join(workdirs[0], "moved-on.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the world seeded at side holds moved-on.txt (err=%v); the two worlds interfered", err)
	}
	if _, err := os.Stat(filepath.Join(workdirs[1], "moved-on.txt")); err != nil {
		t.Fatalf("the world seeded at HEAD lost moved-on.txt: %v", err)
	}
}

// TestRetained_SeedFromAndBaseRefRefusedAtTheSwitch is defence in depth for the
// exclusivity api.go enforces: reached directly (as a future in-package caller
// could), the basis switch refuses the pair instead of letting the seed_from arm
// silently win and drop the caller's ref.
func TestRetained_SeedFromAndBaseRefRefusedAtTheSwitch(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	rt := RetainedRuntime{
		Runner: func(context.Context, string, SubAgentParams) (string, error) {
			t.Error("the sub-agent ran: the basis pair must be refused before any world exists")
			return "", nil
		},
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		SessionID:    "sess-baseref-exclusive",
		Proposals:    NewProposalRegistry(),
	}
	_, err := StartRetainedRun(context.Background(), rt, "go", SubAgentParams{Role: "developer", SeedFrom: "proposal:1:1", BaseRef: "side"}, 10*time.Second, false)
	if err == nil {
		t.Fatal("seed_from + base_ref = nil error, want a refusal")
	}
	for _, want := range []string{"both name a basis", "seed_from", "base_ref"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}
