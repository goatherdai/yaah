package recovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitForTest(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
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

// TestWorktreeOwnedByRealFixture mirrors the janitor's
// TestCollectorReclaimsSettledWorktree fixture (a real git repo, a real
// worktree, then a deregistered admin dir) and asserts ownership still holds.
//
// This is the cross-platform regression that a synthetic test cannot cover:
// git writes the worktree's gitdir in the physical/canonical form, while the
// recorded repo path comes from t.TempDir — a symlinked path on macOS
// (/var -> /private/var) and an 8.3 short-name path on Windows. The two
// spellings differ, so ownership must be decided by filesystem identity,
// not a string-prefix compare. If WorktreeOwnedBy regresses to a lexical
// check, this test fails on macOS/Windows while passing on Linux.
func TestWorktreeOwnedByRealFixture(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitForTest(t, repo)
	wt := filepath.Join(dir, "wt-settled")
	addCmd := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	// Deregister: remove the worktree's admin entry, keep the checkout and
	// its .git file (the teardown-missed orphan). Read the gitdir from the
	// .git file so we remove exactly the entry git created.
	data, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: "))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wt, gitdir)
	}
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	pruneCmd := exec.Command("git", "-C", repo, "worktree", "prune")
	if out, err := pruneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree prune: %v\n%s", err, out)
	}

	if !WorktreeOwnedBy(repo, wt) {
		t.Errorf("WorktreeOwnedBy(repo, wt) = false, want true (deregistered orphan; gitdir spelling may differ from repo path)")
	}
}
