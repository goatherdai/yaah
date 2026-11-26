package substrate

// git_write_refusal_test.go covers ConfinedGitWriteRefusal, the one statement of
// the git-write rule shared by the git tool's pre-execution refusal and the
// exec-time hint (bead yaah-pvm8).
//
// Two properties there are load-bearing and neither is visible from the
// single-root happy path:
//
//   - the boundary is named from the workspace's OWN writable set, so a narrowed
//     grant set gets plural-root wording instead of a single root it does not
//     have (the defects exec's grantsHint already fixes one function away);
//   - the two surfaces stay distinguishable, because a parent settling a dispatch
//     reads "the tool declined" and "the kernel refused" as different evidence,
//     and the codebase treats that distinction as evidence quality.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goatherdai/goatherd-runtime/sandbox/containment"
)

// testGitWriters stands in for the git tool's table-derived list: what matters
// here is that the caller's list reaches the message verbatim.
const testGitWriters = "add, commit, fetch, pull, push"

func TestConfinedGitWriteRefusal_UnconfinedWorkspaceCanWrite(t *testing.T) {
	root := t.TempDir()
	pv := NewPathValidator(root, false, nil)
	pv.WorkDir = pv.WorkspaceRoot
	ws := newLocalWorkspace(pv)
	if ws.Confined() {
		t.Fatal("the fixture must be an unconfined workspace")
	}
	if got := ConfinedGitWriteRefusal(ws, testGitWriters, true); got != "" {
		t.Errorf("a write can land in an unconfined workspace, so there is no refusal to make: %q", got)
	}
	if got := ConfinedGitWriteRefusal(nil, testGitWriters, true); got != "" {
		t.Errorf("a nil workspace must produce no refusal, got %q", got)
	}
}

func TestConfinedGitWriteRefusal_SurfacesAreDistinguishable(t *testing.T) {
	root := t.TempDir()
	ws := confinedTestWorkspace(root, fakeConfinementBackend{}, containment.PlacementPaddock)

	pre := ConfinedGitWriteRefusal(ws, testGitWriters, true)
	if pre == "" {
		t.Fatal("a confined workspace cannot land a git write; the pre-exec surface must refuse")
	}
	// Nothing was refused by the sandbox on this surface: the tool declined
	// before it spawned git, and saying otherwise misreports which gate acted.
	if strings.Contains(pre, "refused by the sub-agent sandbox") {
		t.Errorf("the pre-exec refusal must not claim the sandbox refused a write it never attempted:\n%s", pre)
	}
	if !strings.Contains(pre, "before running git") {
		t.Errorf("the pre-exec refusal must say the tool declined before git ran:\n%s", pre)
	}

	post := ConfinedGitWriteRefusal(ws, testGitWriters, false)
	if !strings.Contains(post, "refused by the sub-agent sandbox") {
		t.Errorf("the exec-time hint reports a real kernel denial and must say so:\n%s", post)
	}

	// One rule on both surfaces: the body, the caller's action list and the
	// commit rule must all be present, so neither surface can drift into its own
	// account of the same boundary.
	for _, want := range []string{
		"git's index and object store live in the original repository",
		"git write commands (" + testGitWriters + ")",
		"orchestrator commits",
	} {
		if !strings.Contains(pre, want) || !strings.Contains(post, want) {
			t.Errorf("both surfaces must state %q\npre:  %s\npost: %s", want, pre, post)
		}
	}
}

func TestConfinedGitWriteRefusal_NamesTheWorkspacesOwnRoots(t *testing.T) {
	root := t.TempDir()
	base := confinedTestWorkspace(root, fakeConfinementBackend{}, containment.PlacementPaddock)

	single := ConfinedGitWriteRefusal(base, testGitWriters, true)
	if !strings.Contains(single, "whose only writable root is "+root) {
		t.Errorf("the legacy lane has one writable root and must name it:\n%s", single)
	}

	// Restrictive grants: the jail's writable set is a list now, so the
	// single-root sentence would name a boundary this workspace does not have.
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	narrowed := base.WithWritableRoots([]string{a, b})
	got := ConfinedGitWriteRefusal(narrowed, testGitWriters, true)
	if !strings.Contains(got, "whose writable roots are "+a+", "+b) {
		t.Errorf("a narrowed workspace must name its grant list:\n%s", got)
	}
	if strings.Contains(got, "only writable root") {
		t.Errorf("a narrowed workspace must not claim a single writable root:\n%s", got)
	}

	// A declared all-readonly grant set has no writable root at all, so naming
	// the worktree as the writable root would be false there too.
	readOnly := base.WithWritableRoots(nil)
	got = ConfinedGitWriteRefusal(readOnly, testGitWriters, true)
	if !strings.Contains(got, "declares no writable root") {
		t.Errorf("a read-only grant set must say it has no writable root:\n%s", got)
	}
	if strings.Contains(got, "only writable root") {
		t.Errorf("a read-only grant set must not claim a writable root:\n%s", got)
	}
}
