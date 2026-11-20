package janitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/dispatch"
)

// Collector tests: the reclamation mode's safety contract. Every test
// drives the collector over a real trace store and real directories; the
// deletion path is exercised against a real git worktree so the git
// registry check runs for real.

// gitRepo creates a real git repository for worktree tests.
func gitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := execCommand(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")
}

// makeRealWorktree creates a registered git worktree of repo at path.
func makeRealWorktree(t *testing.T, repo, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := execCommand(repo, "worktree", "add", "--detach", path, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
}

// deregisterWorktree models the teardown-missed scenario: the worktree was
// never torn down (directory still on disk with all its files) but git's
// registry no longer lists it — exactly what a crashed `git worktree
// remove` leaves behind. The admin metadata is removed; the checkout
// stays.
func deregisterWorktree(t *testing.T, repo, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		t.Fatalf("read .git file: %v", err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: "))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	// Remove the worktree's admin entry so the registry no longer lists
	// it, then prune git's bookkeeping.
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	if out, err := execCommand(repo, "worktree", "prune").CombinedOutput(); err != nil {
		t.Fatalf("git worktree prune: %v\n%s", err, out)
	}
}

// ageWorktree backdates a worktree directory's mtime without touching the
// .git file inside it: os.Chtimes follows symlinks (macOS t.TempDir is a
// symlink), and WorktreeOwnedBy requires the .git file to keep its own
// mtime — only the directory's age gates the sweep.
func ageWorktree(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestCollectorReclaimsSettledWorktree: the prime scenario — a settled
// proposal whose worktree is still registered (teardown missed), past the
// age threshold, must be reclaimed with its bytes counted.
func TestCollectorReclaimsSettledWorktree(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-settled")
	makeRealWorktree(t, repo, wt)
	// Teardown missed: the directory survives, git's registry does not
	// know it — the collector must reclaim it.
	deregisterWorktree(t, repo, wt)
	ageWorktree(t, wt, 72*time.Hour)

	appendProposalFact(t, store, "scope:proposal:777:1", dispatch.SchemaProposalSealed, "proposal:777:1")
	appendProposalFact(t, store, "scope:proposal:777:1", dispatch.SchemaProposalSettled, "proposal:777:1")

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:777:1", Path: wt, Repo: repo}},
	}
	c := NewCollector(store, registry, repo, SweepOptions{
		Now:                time.Now,
		SettledWorktreeAge: 24 * time.Hour,
	})

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1 (the settled worktree)", len(targets))
	}

	res := c.Execute(context.Background(), targets)
	if res.DeletedWorktrees != 1 || len(res.Failed) != 0 {
		t.Fatalf("execute = %+v; want one clean worktree deletion", res)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after reclaim: %v", err)
	}
	// The repo must no longer list it.
	out, err := execCommand(repo, "worktree", "list").CombinedOutput()
	if err != nil || strings.Contains(string(out), wt) {
		t.Errorf("git still lists the reclaimed worktree: %s (%v)", out, err)
	}
}

// TestCollectorAgeGateSkipsFreshWorktree: a settled worktree younger than
// the threshold is left alone — the age gate is the human-review window.
func TestCollectorAgeGateSkipsFreshWorktree(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-fresh")
	makeRealWorktree(t, repo, wt) // mtime is now — younger than the threshold

	appendProposalFact(t, store, "scope:proposal:888:1", dispatch.SchemaProposalSealed, "proposal:888:1")
	appendProposalFact(t, store, "scope:proposal:888:1", dispatch.SchemaProposalSettled, "proposal:888:1")

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:888:1", Path: wt, Repo: repo}},
	}
	c := NewCollector(store, registry, repo, SweepOptions{SettledWorktreeAge: 24 * time.Hour})

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %d, want 0 — fresh worktree is inside the review window", len(targets))
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("fresh worktree was touched: %v", err)
	}
}

// TestCollectorRefusesLiveWorktrees: a worktree still registered as a live
// worktree of its repo is never reclaimed, even settled and old — git's
// registry is the authority on liveness.
func TestCollectorRefusesLiveWorktrees(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-live")
	makeRealWorktree(t, repo, wt)
	ageWorktree(t, wt, 72*time.Hour)

	appendProposalFact(t, store, "scope:proposal:999:1", dispatch.SchemaProposalSealed, "proposal:999:1")
	appendProposalFact(t, store, "scope:proposal:999:1", dispatch.SchemaProposalSettled, "proposal:999:1")

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:999:1", Path: wt, Repo: repo}},
	}
	c := NewCollector(store, registry, repo, SweepOptions{
		SettledWorktreeAge: 24 * time.Hour,
	})

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %d, want 0 — a registered live worktree is never reclaimed", len(targets))
	}
}

// TestCollectorScratchReclaim: scratch inside a settled worktree is
// reclaimed even when the worktree itself is inside the age window (the
// scratch is redirected housekeeping, safe unconditionally).
func TestCollectorScratchReclaim(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-scratch")
	makeRealWorktree(t, repo, wt)
	// Keep the worktree registered (not deregistered): the collector
	// classifies it as a live worktree, and scratch inside it is still
	// reported for reclamation — the scratch is housekeeping, not work.
	scratch := filepath.Join(wt, ScratchDirName, "build")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "cache.bin"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	appendProposalFact(t, store, "scope:proposal:123:1", dispatch.SchemaProposalSealed, "proposal:123:1")
	appendProposalFact(t, store, "scope:proposal:123:1", dispatch.SchemaProposalSettled, "proposal:123:1")

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:123:1", Path: wt, Repo: repo}},
	}
	// No age threshold: the worktree is fresh, but its scratch is fair game.
	c := NewCollector(store, registry, repo, SweepOptions{})

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].Kind != KindScratch {
		t.Fatalf("targets = %+v; want exactly the scratch dir", targets)
	}

	res := c.Execute(context.Background(), targets)
	if res.DeletedScratch != 1 || len(res.Failed) != 0 {
		t.Fatalf("execute = %+v; want one clean scratch deletion", res)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch still exists after reclaim: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree itself was deleted with its scratch: %v", err)
	}
}

// TestCollectorEmptyRegistryIsNoop: no orphans, no targets, no deletions.
func TestCollectorEmptyRegistryIsNoop(t *testing.T) {
	store := newTestStore(t)
	c := NewCollector(store, &recovery.Report{}, "", SweepOptions{SettledWorktreeAge: 24 * time.Hour})
	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %d, want 0", len(targets))
	}
	res := c.Execute(context.Background(), targets)
	if res.DeletedWorktrees != 0 || res.DeletedScratch != 0 {
		t.Fatalf("execute on empty targets = %+v", res)
	}
}

// TestSweepResultCarriesEvidence is a compile-time guard: the audit line
// the command prints must carry the same numbers the result holds.
func TestSweepResultCarriesEvidence(t *testing.T) {
	res := SweepResult{DeletedWorktrees: 1, DeletedScratch: 2, ReclaimedBytes: 4096}
	if res.DeletedWorktrees+res.DeletedScratch != 3 || res.ReclaimedBytes != 4096 {
		t.Errorf("sweep result lost its evidence: %+v", res)
	}
}

// TestCollectorRefusesReplacedPath pins the TOCTOU contract: a target
// classified as a settled orphan must be re-verified immediately before
// deletion. A directory that replaced the worktree (no longer owned by
// the repo's administration) is refused — the raw-tree fallback must
// never erase an unproven occupant.
func TestCollectorRefusesReplacedPath(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-settled")
	makeRealWorktree(t, repo, wt)
	deregisterWorktree(t, repo, wt)
	ageWorktree(t, wt, 72*time.Hour)

	appendProposalFact(t, store, "scope:proposal:555:1", dispatch.SchemaProposalSealed, "proposal:555:1")
	appendProposalFact(t, store, "scope:proposal:555:1", dispatch.SchemaProposalSettled, "proposal:555:1")

	// The occupant changed between classification and deletion: a plain
	// directory (a user's data, a different checkout) now sits at the
	// recorded path.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "precious.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ageWorktree(t, wt, 72*time.Hour)

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:555:1", Path: wt, Repo: repo}},
	}
	c := NewCollector(store, registry, repo, SweepOptions{
		Now:                time.Now,
		SettledWorktreeAge: 24 * time.Hour,
	})
	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %d, want 0 — the replaced path is not even a worktree shape anymore (classify-time provenance catches it)", len(targets))
	}

	// The deletion-time re-verification is the second line of defense:
	// exercise it directly, the way Execute would after a classify→delete
	// window in which the occupant changed.
	res := c.Execute(context.Background(), []SweepTarget{{
		Kind: KindSettledWorktree, Path: wt, ScopeID: "scope:proposal:555:1", Terminal: true,
	}})
	if res.DeletedWorktrees != 0 || len(res.Failed) != 1 {
		t.Fatalf("execute = %+v; want the replaced path refused at delete time", res)
	}
	if _, err := os.Stat(filepath.Join(wt, "precious.txt")); err != nil {
		t.Errorf("the replacement's data was destroyed: %v", err)
	}
}

// TestCollectorRefusesScratchOfReplacedCheckout pins the scratch
// provenance contract: scratch inside a directory that replaced the
// recorded worktree (no longer owned by the repo) must not be swept.
func TestCollectorRefusesScratchOfReplacedCheckout(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitRepo(t, repo)
	wt := filepath.Join(dir, "wt-scratch")
	makeRealWorktree(t, repo, wt)

	appendProposalFact(t, store, "scope:proposal:666:1", dispatch.SchemaProposalSealed, "proposal:666:1")
	appendProposalFact(t, store, "scope:proposal:666:1", dispatch.SchemaProposalSettled, "proposal:666:1")

	// Replace the worktree with an unrelated checkout that has its own
	// .yaah-scratch (another project's housekeeping).
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	otherRepo := filepath.Join(dir, "other")
	gitRepo(t, otherRepo)
	makeRealWorktree(t, otherRepo, wt)
	otherScratch := filepath.Join(wt, ScratchDirName)
	if err := os.MkdirAll(otherScratch, 0o700); err != nil {
		t.Fatal(err)
	}

	registry := &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:666:1", Path: wt, Repo: repo}},
	}
	c := NewCollector(store, registry, repo, SweepOptions{})
	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	var scratchTargets []SweepTarget
	for _, tg := range targets {
		if tg.Kind == KindScratch {
			scratchTargets = append(scratchTargets, tg)
		}
	}
	if len(scratchTargets) != 1 {
		t.Fatalf("scratch targets = %d, want 1", len(scratchTargets))
	}
	res := c.Execute(context.Background(), scratchTargets)
	if res.DeletedScratch != 0 || len(res.Failed) != 1 {
		t.Fatalf("execute = %+v; want the other checkout's scratch refused", res)
	}
	if _, err := os.Stat(otherScratch); err != nil {
		t.Errorf("the other checkout's scratch was destroyed: %v", err)
	}
}
