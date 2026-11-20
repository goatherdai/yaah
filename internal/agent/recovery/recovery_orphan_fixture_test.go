package recovery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// TestCollectorReclaimsSettledWorktree fixture (real git repo, real
// worktree, deregistered admin dir, aged directory) and asserts ownership,
// dumping every intermediate the comparison uses when it fails.
func TestWorktreeOwnedByRealFixture(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitForTest(t, repo)
	wt := filepath.Join(dir, "wt-settled")
	addCmd := exec.Command("git", "-C", repo, "worktree", "add", "--detach", wt, "HEAD")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	// Deregister: remove the admin dir, keep the checkout.
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

	dump := func(msg string) {
		t.Logf("DEBUG %s", msg)
	}

	// Age the worktree directory. Note: os.Chtimes follows symlinks, so on
	// macOS this sets the mtime of the worktree's real directory —
	// matching what the janitor sees (its own os.Stat also follows
	// symlinks). The .git file's mtime is irrelevant to the ownership
	// check; nothing sets it deliberately.
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(wt, old, old); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(wt); err == nil {
		dump(fmt.Sprintf("wt stat: mtime set %v ago", time.Since(st.ModTime()).Round(time.Second)))
	}

	dump(".git file: " + string(data))
	dump("gitdir: " + gitdir)
	if st, err := os.Stat(gitdir); err != nil {
		dump("gitdir stat: " + err.Error())
	} else {
		dump(fmt.Sprintf("gitdir stat: exists (mtime set %v ago)", time.Since(st.ModTime()).Round(time.Second)))
	}
	if resolved, err := filepath.EvalSymlinks(gitdir); err != nil {
		dump("gitdir EvalSymlinks: " + err.Error())
	} else {
		dump("gitdir EvalSymlinks: " + resolved)
	}
	owned0 := filepath.Clean(filepath.Join(repo, ".git", "worktrees"))
	if resolved, err := filepath.EvalSymlinks(owned0); err == nil {
		owned0 = resolved
	}
	dump("owned (resolved): " + owned0)
	if resolved, ok := resolveMissingAncestors(filepath.Clean(gitdir)); ok {
		dump("gitdir fallback: " + resolved)
	} else {
		dump("gitdir fallback: none")
	}

	if !WorktreeOwnedBy(repo, wt) {
		t.Errorf("WorktreeOwnedBy(repo, wt) = false; see DEBUG dumps above")
	}
}
