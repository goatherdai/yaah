package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorktreeOwnedByOrphanSymlinkedPath is the macOS/Windows regression
// scenario: git wrote the worktree's gitdir in the symlinked form while
// the recorded paths pass through a symlinked root (macOS /var ->
// /private/var, Windows temp-dir short names). When the worktree is
// deregistered and its admin entry removed (the teardown-missed orphan),
// the gitdir no longer resolves, so a lexical prefix compare — where
// `owned` resolves through symlinks but `gitdir` cannot — refuses a true
// owner. Ownership must accept it via ancestor resolution.
func TestWorktreeOwnedByOrphanSymlinkedPath(t *testing.T) {
	base := t.TempDir()
	// A symlinked root standing in for the symlinked platform temp dir:
	// git writes physical paths, the recorded repo path is the symlinked
	// form.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	repo := filepath.Join(link, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The worktrees admin dir, then a fake orphan worktree: a .git file
	// written in the SYMLINKED form (the failing case), nothing else.
	admin := filepath.Join(repo, ".git", "worktrees")
	if err := os.MkdirAll(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(link, "wt-orphan")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	// gitdir: <symlinked repo>/.git/worktrees/wt-orphan — and we then
	// remove the admin entry, leaving the checkout and the .git file.
	if err := os.WriteFile(filepath.Join(wt, ".git"),
		[]byte("gitdir: "+filepath.Join(link, "repo", ".git", "worktrees", "wt-orphan")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, ".git", "worktrees", "wt-orphan")); err != nil {
		t.Fatal(err)
	}

	if !WorktreeOwnedBy(repo, wt) {
		t.Errorf("WorktreeOwnedBy(repo, orphan) = false, want true (symlinked-form gitdir, unresolvable after removal)")
	}
}

// TestWorktreeOwnedByResolveMissingAncestors exercises the ancestor
// resolution helper directly: a missing tail over an existing physical
// ancestor resolves; a fully missing chain does not.
func TestWorktreeOwnedByResolveMissingAncestors(t *testing.T) {
	base := t.TempDir()
	existing := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	missingTail := filepath.Join(existing, "c", "d")
	got, ok := resolveMissingAncestors(missingTail)
	if !ok {
		t.Fatalf("resolveMissingAncestors(%q) ok=false, want true", missingTail)
	}
	if got != missingTail {
		t.Errorf("resolveMissingAncestors = %q, want %q (existing physical ancestor + tail)", got, missingTail)
	}

	// The deepest-existing-ancestor rule: walk until a component exists,
	// resolve it, reattach the tail.
	missing2 := filepath.Join(existing, "x", "y")
	got2, ok := resolveMissingAncestors(missing2)
	if !ok {
		t.Fatalf("resolveMissingAncestors(%q) ok=false, want true", missing2)
	}
	if got2 != missing2 {
		t.Errorf("resolveMissingAncestors = %q, want %q", got2, missing2)
	}
	if _, err := os.Stat(missing2); err == nil {
		t.Fatalf("test setup: %q should not exist", missing2)
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

	// .git file not in the worktrees admin namespace.
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

	// main repo root is not owned (the prefix check must exclude the
	// repository's own .git directory root).
	if WorktreeOwnedBy(repo, repo) {
		t.Errorf("WorktreeOwnedBy(repo, repo) = true, want false")
	}

	if !strings.HasPrefix(filepath.Join(repo, ".git", "worktrees")+string(filepath.Separator), filepath.Join(repo, ".git", "worktrees")+string(filepath.Separator)) {
		t.Fatal("prefix identity invariant broken")
	}
}
