package dispatch

// baseref_test.go covers the base_ref resolver (yaah-witj increment B): the
// allowlist gate that runs before any argv, and the resolution itself against a
// real repository.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// TestValidateBaseRef pins the allowlist gate. Every case here must be refused
// BEFORE a git argument list exists: a leading '-' would be read as a flag of
// the subcommand, and a revision expression (HEAD~3, main@{1}, A..B) is not
// what the parameter means even when git would happily resolve it.
func TestValidateBaseRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string // substring of the refusal; "" means accept
	}{
		{name: "branch", ref: "main"},
		{name: "remote-tracking branch", ref: "origin/main"},
		{name: "tag", ref: "v1.2.3"},
		{name: "full commit sha", ref: "0c54a79a1b2c3d4e5f60718293a4b5c6d7e8f901"},
		{name: "abbreviated sha", ref: "0c54a79"},
		{name: "refs path", ref: "refs/heads/feat/x-y_z.1"},
		{name: "HEAD", ref: "HEAD"},
		// `+` is legal in a git ref name and has no revision-expression
		// meaning, so refusing it would refuse a real branch.
		{name: "plus in a branch", ref: "feature/c++"},

		{name: "leading dash", ref: "-D", want: "contains characters a ref name cannot hold"},
		{name: "flag-shaped", ref: "--verify", want: "contains characters a ref name cannot hold"},
		{name: "whitespace", ref: "main branch", want: "contains characters a ref name cannot hold"},
		{name: "tilde", ref: "HEAD~3", want: "contains characters a ref name cannot hold"},
		{name: "caret", ref: "HEAD^{tree}", want: "contains characters a ref name cannot hold"},
		{name: "reflog", ref: "main@{1}", want: "contains characters a ref name cannot hold"},
		{name: "path spec", ref: "HEAD:README.md", want: "contains characters a ref name cannot hold"},
		{name: "range", ref: "main..side", want: "is a revision range"},
		{name: "shell metacharacters", ref: "main;rm -rf", want: "contains characters a ref name cannot hold"},
		// A non-ASCII ref name is legal in git but outside this allowlist (the
		// one class whose bytes depend on normalization); the refusal says so
		// rather than blaming revision syntax.
		{name: "non-ascii ref", ref: "brànch", want: "a non-ASCII ref name is refused"},
		{name: "empty", ref: "", want: "is empty"},
		{name: "path", ref: "/etc/passwd", want: "looks like a filesystem path"},
		{name: "too long", ref: strings.Repeat("a", maxBaseRefBytes+1), want: "the maximum is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBaseRef(tc.ref)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateBaseRef(%q) = %v, want it accepted", tc.ref, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateBaseRef(%q) = nil, want a refusal containing %q", tc.ref, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateBaseRef(%q) = %q, want it to contain %q", tc.ref, err, tc.want)
			}
		})
	}
}

// baseRefFixture builds a repository with a moved-on main, a `side` branch at
// the first commit, and an annotated tag on it, and returns the three
// identities a resolution test needs.
func baseRefFixture(t *testing.T) (repo, first, second string) {
	t.Helper()
	repo = newTestGitRepo(t)
	first = revParseTest(t, repo, "HEAD")
	writeTestFile(t, repo, "second.txt", "second\n")
	mustRunGit(t, repo, "add", "-A")
	mustRunGit(t, repo, "commit", "-m", "second")
	second = revParseTest(t, repo, "HEAD")
	mustRunGit(t, repo, "branch", "side", first)
	mustRunGit(t, repo, "tag", "-a", "ann", "-m", "annotated tag", first)
	return repo, first, second
}

// revParseTest runs `git -C dir rev-parse <rev>` and fails on error.
func revParseTest(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// TestResolveBaseRef covers the resolution path against a real repository.
func TestResolveBaseRef(t *testing.T) {
	ctx := context.Background()

	t.Run("HEAD resolves to the current commit and its tree", func(t *testing.T) {
		repo, _, second := baseRefFixture(t)
		commit, err := resolveBaseRef(ctx, repo, "HEAD")
		if err != nil {
			t.Fatalf("resolveBaseRef: %v", err)
		}
		if commit.Commit != second {
			t.Fatalf("commit = %q, want HEAD %q", commit.Commit, second)
		}
		if want := revParseTest(t, repo, second+"^{tree}"); commit.Tree != want {
			t.Fatalf("tree = %q, want %q", commit.Tree, want)
		}
		if commit.Ref != "HEAD" {
			t.Fatalf("ref = %q, want the requested spelling", commit.Ref)
		}
	})

	t.Run("a branch resolves to the commit it names, not the repository HEAD", func(t *testing.T) {
		repo, first, _ := baseRefFixture(t)
		commit, err := resolveBaseRef(ctx, repo, "side")
		if err != nil {
			t.Fatalf("resolveBaseRef: %v", err)
		}
		if commit.Commit != first {
			t.Fatalf("commit = %q, want side %q", commit.Commit, first)
		}
	})

	// The peel is load-bearing: `rev-parse --verify <tag>` answers the TAG
	// OBJECT sha for an annotated tag, and a synthesized state carrying a tag
	// object as its head is not the commit the dispatch should start from.
	t.Run("an annotated tag peels to its commit", func(t *testing.T) {
		repo, first, _ := baseRefFixture(t)
		tagObject := revParseTest(t, repo, "ann")
		if tagObject == first {
			t.Fatal("fixture is not an annotated tag: rev-parse ann returned the commit")
		}
		commit, err := resolveBaseRef(ctx, repo, "ann")
		if err != nil {
			t.Fatalf("resolveBaseRef: %v", err)
		}
		if commit.Commit == tagObject {
			t.Fatalf("commit = %q, the TAG OBJECT sha; the ref must peel through ^{commit}", commit.Commit)
		}
		if commit.Commit != first {
			t.Fatalf("commit = %q, want the tagged commit %q", commit.Commit, first)
		}
	})

	t.Run("an abbreviated object name resolves", func(t *testing.T) {
		repo, first, _ := baseRefFixture(t)
		commit, err := resolveBaseRef(ctx, repo, first[:8])
		if err != nil {
			t.Fatalf("resolveBaseRef: %v", err)
		}
		if commit.Commit != first {
			t.Fatalf("commit = %q, want %q", commit.Commit, first)
		}
	})

	t.Run("an unknown ref names the input and the repository", func(t *testing.T) {
		repo, _, _ := baseRefFixture(t)
		_, err := resolveBaseRef(ctx, repo, "no-such-branch")
		if err == nil {
			t.Fatal("resolveBaseRef on an unknown ref = nil error")
		}
		for _, want := range []string{`"no-such-branch"`, "does not resolve to a commit"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to contain %q", err, want)
			}
		}
	})

	// A dispatch world cannot fetch (git fetch writes the shared object store),
	// so an unfetched remote-tracking ref must be told apart from a typo: the
	// caller's next move is `git fetch` in the parent, not a spelling fix.
	t.Run("an unfetched remote-tracking ref gets its own message", func(t *testing.T) {
		repo, _, _ := baseRefFixture(t)
		mustRunGit(t, repo, "remote", "add", "origin", repo)
		_, err := resolveBaseRef(context.Background(), repo, "origin/main")
		if err == nil {
			t.Fatal("resolveBaseRef on an unfetched remote-tracking ref = nil error")
		}
		for _, want := range []string{"has not fetched locally", "fetch it in the parent repository"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to contain %q", err, want)
			}
		}
	})

	// A remote PREFIX match is not proof the ref is merely unfetched: a typo
	// matches exactly as an unfetched branch does, and telling that caller to
	// fetch gives advice that can never work and repeats forever. When the
	// remote has refs locally but not this name, the message must stop claiming
	// "not fetched" and point at the spelling.
	t.Run("a typo in a remote-tracking ref is not called merely unfetched", func(t *testing.T) {
		repo, _, _ := baseRefFixture(t)
		mustRunGit(t, repo, "remote", "add", "origin", repo)
		mustRunGit(t, repo, "fetch", "origin")
		// The remote's own refs must exist locally for the case to be the one it
		// claims to be: a name that is NOT among them.
		if out := mustGitIn(t, repo, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/"); out == "" {
			t.Fatal("fixture did not fetch any refs/remotes/origin ref")
		}
		_, err := resolveBaseRef(context.Background(), repo, "origin/mian")
		if err == nil {
			t.Fatal("resolveBaseRef on a typo'd remote-tracking ref = nil error")
		}
		for _, want := range []string{"holds no such ref", "check the spelling", `"origin"`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to contain %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "has not fetched locally") {
			t.Fatalf("error = %q, claiming the ref was merely never fetched; fetching cannot fix a typo", err)
		}
	})

	t.Run("a ref expression never reaches git", func(t *testing.T) {
		repo, _, _ := baseRefFixture(t)
		if _, err := resolveBaseRef(ctx, repo, "HEAD~1"); err == nil {
			t.Fatal("resolveBaseRef(HEAD~1) = nil error, want a refusal before git runs")
		}
	})

	t.Run("no repository path is refused by name", func(t *testing.T) {
		_, err := resolveBaseRef(ctx, "", "main")
		if err == nil || !strings.Contains(err.Error(), "no repository path") {
			t.Fatalf("err = %v, want a missing-repository refusal", err)
		}
	})
}

// TestBaseRefWorkspaceState pins the synthesized state's SHAPE against what the
// runtime records for a git capture. The kernel Apply refuses a state carrying
// neither tree_sha nor stash_sha, and requires head_sha, so the shape is a
// contract rather than a convenience.
func TestBaseRefWorkspaceState(t *testing.T) {
	repo, first, _ := baseRefFixture(t)
	commit, err := resolveBaseRef(context.Background(), repo, "side")
	if err != nil {
		t.Fatalf("resolveBaseRef: %v", err)
	}
	state := commit.workspaceState()
	if state.Backend != "git" {
		t.Fatalf("backend = %q, want git", state.Backend)
	}
	if state.Revision != first {
		t.Fatalf("revision = %q, want %q", state.Revision, first)
	}
	// The keys are pinned through the const pair, not as literals: baseref.go is
	// yaah's first WRITE of the kernel's Data layout (every other site reads a
	// state an upstream capture produced), so the writer and the six readers
	// must name the layout through one place. The literals are asserted below as
	// well, so the pair cannot drift from what the kernel actually records.
	if workspaceStateHeadKey != "head_sha" || workspaceStateTreeKey != "tree_sha" {
		t.Fatalf("Data keys = %q/%q, want the kernel's head_sha/tree_sha", workspaceStateHeadKey, workspaceStateTreeKey)
	}
	if got, _ := state.Data[workspaceStateHeadKey].(string); got != first {
		t.Fatalf("head_sha = %q, want %q", got, first)
	}
	if got, _ := state.Data[workspaceStateTreeKey].(string); got != commit.Tree {
		t.Fatalf("tree_sha = %q, want the resolved tree %q", got, commit.Tree)
	}
	// basisRecord is increment A's one rendering of a state; the synthesized
	// state must be legible through it without a special case, or the recorded
	// basis of a base_ref dispatch would come out empty.
	rec := basisRecord(state)
	if rec == nil || rec.Head != first || rec.Tree == "" {
		t.Fatalf("basisRecord(state) = %+v, want head %q and a tree", rec, first)
	}
	// And it must pair through freshForkSeed, which is what makes the seed and
	// the baseline the same state by construction.
	seed := freshForkSeed(state)
	if seed.seed.Backend == "" || seed.resolvedBaseline().Revision != first {
		t.Fatalf("freshForkSeed(state) resolved baseline = %+v, want the same state", seed.resolvedBaseline())
	}
}
