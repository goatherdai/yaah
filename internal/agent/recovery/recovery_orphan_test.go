package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWorktreeOwnedByOrphanSymlinkedPath is the macOS regression scenario:
// git wrote the worktree's gitdir in the PHYSICAL form while the recorded
// repo path passes through a symlinked root (macOS /var -> /private/var).
// The worktree is deregistered (admin entry removed), so the gitdir no
// longer resolves; a lexical path-prefix compare — where the two sides are
// spelled differently — refuses a true owner. Ownership is decided by
// filesystem identity instead, so it must accept this case.
func TestWorktreeOwnedByOrphanSymlinkedPath(t *testing.T) {
	base := t.TempDir() // physical form
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The repository is provisioned and recorded through the symlinked
	// root; git writes the worktree's gitdir through the physical root.
	repo := filepath.Join(link, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(link, "wt-orphan")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	// gitdir: <physical repo>/.git/worktrees/wt-orphan. The admin dir is
	// then removed (deregistered orphan), leaving the checkout + .git file.
	if err := os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+filepath.Join(base, "repo", ".git", "worktrees", "wt-orphan")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !WorktreeOwnedBy(repo, wt) {
		t.Errorf("WorktreeOwnedBy(repo, orphan) = false, want true (physical-form gitdir vs symlinked repo path)")
	}
}

// TestWorktreeOwnedByNegativeCases: impostors and non-worktrees stay
// unowned on every platform.
func TestWorktreeOwnedByNegativeCases(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Plain directory: no .git file.
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if WorktreeOwnedBy(repo, plain) {
		t.Errorf("WorktreeOwnedBy(plain) = true, want false")
	}

	// .git file pointing outside any worktrees admin namespace.
	sneaky := filepath.Join(dir, "sneaky")
	if err := os.MkdirAll(sneaky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sneaky, ".git"), []byte("gitdir: "+filepath.Join(dir, "elsewhere")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if WorktreeOwnedBy(repo, sneaky) {
		t.Errorf("WorktreeOwnedBy(outside-admin) = true, want false")
	}

	// Impostor pointing into the SAME repo's .git but not under worktrees:
	// filesystem identity alone would accept it (same .git dir), so the
	// shape check must reject it. This is why ownership is identity AND
	// shape, not just one of them.
	insideGit := filepath.Join(dir, "inside-git")
	if err := os.MkdirAll(insideGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(insideGit, ".git"),
		[]byte("gitdir: "+filepath.Join(repo, ".git", "refs", "heads", "main")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if WorktreeOwnedBy(repo, insideGit) {
		t.Errorf("WorktreeOwnedBy(inside-.git-not-worktrees) = true, want false")
	}

	// A different repository's worktree admin dir must not count as owned,
	// even though the shape is identical.
	otherRepo := filepath.Join(dir, "other-repo")
	if err := os.MkdirAll(filepath.Join(otherRepo, ".git", "worktrees"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".git"),
		[]byte("gitdir: "+filepath.Join(otherRepo, ".git", "worktrees", "wt-x")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if WorktreeOwnedBy(repo, foreign) {
		t.Errorf("WorktreeOwnedBy(other repo's worktree) = true, want false")
	}

	// The main repo root is not a worktree of itself (no .git file there).
	if WorktreeOwnedBy(repo, repo) {
		t.Errorf("WorktreeOwnedBy(repo, repo) = true, want false")
	}
}
