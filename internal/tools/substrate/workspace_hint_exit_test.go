package substrate

// workspace_hint_exit_test.go pins the exec-time half of the confinement hint:
// WHEN it attaches. The hint's matching is tested as pure string work in
// internal/tools/confinement_test.go; what regressed (bead yaah-pvm8) is the
// gate in confinedExec, which used to attach it only when the whole command
// exited non-zero — so a composite that ends in success (`git add -A; echo
// rc=$?`, or any `cmd || true`) told the sub-agent nothing and left it staring
// at a bare "Permission denied".
//
// The path runs through a fake containment backend, not a real LSM: the
// question is what confinedExec does with a result, so replaying one is the
// honest fixture, and it keeps the case platform-independent.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goatherdai/goatherd-runtime/sandbox/confinement"
	"github.com/goatherdai/goatherd-runtime/sandbox/containment"
)

// fakeConfinementBackend establishes its paddock unconditionally and replays one
// canned result, standing in for the kernel so the exec→hint path can be driven
// with any exit code and output.
type fakeConfinementBackend struct{ result containment.LaunchResult }

func (f fakeConfinementBackend) Name() string              { return "fake" }
func (f fakeConfinementBackend) EnforcementTier() string   { return "fake" }
func (f fakeConfinementBackend) Available() (bool, string) { return true, "" }
func (f fakeConfinementBackend) SupportsNetwork(confinement.NetMode) bool {
	return true
}
func (f fakeConfinementBackend) ProfileFor([]string, confinement.NetworkPolicy) string { return "" }
func (f fakeConfinementBackend) Probe(string, string, []string) error                  { return nil }

func (f fakeConfinementBackend) Launch(string, containment.LaunchRequest) (containment.LaunchResult, error) {
	return f.result, nil
}

// TestConfinedExec_HintFollowsTheOutputNotTheExitCode is the regression for
// yaah-pvm8: a refusal in a command that exits 0 must still be explained, and
// the twin negatives that made the hint safe must stay empty at exit 0 too.
func TestConfinedExec_HintFollowsTheOutputNotTheExitCode(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	gitRefusal := fmt.Sprintf("fatal: Unable to create '%s': Permission denied",
		filepath.Join(outside, ".git", "worktrees", "wt", "index.lock"))

	cases := []struct {
		name string
		res  containment.LaunchResult
		// separate is the request's SeparateStreams setting: with it, the hint
		// is appended to stderr; without it, to the combined stdout.
		separate bool
		wantHint bool
		// noRoot marks a hint that names no writable root (the snap
		// translation, which is about packaging, not about the boundary); the
		// table's root check applies to the boundary hints only.
		noRoot bool
	}{
		{
			// The hole: `git add -A; echo rc=$?` — the composite succeeds while
			// the index write was refused.
			name:     "zero exit, refusal on stderr",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: "rc=128\n", Stderr: gitRefusal},
			separate: true,
			wantHint: true,
		},
		{
			// A backend that reports combined output leaves git's stderr in
			// Stdout; the hint must not depend on which stream carried it.
			name:     "zero exit, refusal in combined stdout",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: gitRefusal, Stderr: ""},
			wantHint: true,
		},
		{
			// A non-git refusal outside the root still names the boundary.
			name: "zero exit, plain out-of-root refusal",
			res: containment.LaunchResult{ExitCode: 0,
				Stdout: "sh: 1: cannot create " + filepath.Join(outside, "motd") + ": Permission denied\nrc=2\n"},
			separate: true,
			wantHint: true,
		},
		{
			name:     "non-zero exit keeps its hint",
			res:      containment.LaunchResult{ExitCode: 128, Stderr: gitRefusal},
			separate: true,
			wantHint: true,
		},
		// Twin negatives: each of these produced no hint before the change and
		// must still produce none, because widening the gate must not caption
		// ordinary output as a boundary refusal.
		{
			name:     "zero exit, no output",
			res:      containment.LaunchResult{ExitCode: 0},
			separate: true,
			wantHint: false,
		},
		{
			name:     "zero exit, network failure",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: "fatal: unable to access 'https://example.invalid/x': Failed to connect: connection refused\n"},
			wantHint: false,
		},
		{
			name:     "zero exit, permission failure inside the root",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: "cat: " + filepath.Join(root, "secret") + ": Permission denied\n"},
			wantHint: false,
		},
		{
			name:     "zero exit, unattributable refusal",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: "git: Permission denied\n"},
			wantHint: false,
		},
		{
			// The other side of dropping the exit-code gate: a SUCCESSFUL command
			// may merely MENTION the snap token (a grep over this repository's
			// docs, a test log). The token alone is not a failure, so with no
			// exit code left to filter on, the snap branch must require a failure
			// phrasing of its own — otherwise healthy output gets captioned as a
			// sandbox refusal (yaah-pvm8 review, F4).
			name:     "zero exit, output merely mentions snap-confine",
			res:      containment.LaunchResult{ExitCode: 0, Stdout: "docs/notes.md: snap-confine is setuid and needs capabilities\n"},
			wantHint: false,
		},
		{
			// The same token WITH a failure phrasing is a real snap start
			// failure, and must still be translated at exit 0 (the composite case
			// the widened gate exists for). The snap hint names no root, so this
			// case is asserted through the pure function, not the table's
			// root check: see TestConfinementHint_TranslatesSnapConfine.
			name:     "non-zero exit, snap-confine failed to start",
			res:      containment.LaunchResult{ExitCode: 1, Stdout: "snap-confine: failed to start: no_new_privs refused\n"},
			wantHint: true,
			noRoot:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backend := fakeConfinementBackend{result: c.res}
			ws := confinedTestWorkspace(root, backend, containment.PlacementPaddock)

			got, err := ws.Exec(context.Background(), ExecRequest{
				Command: "sh", Args: []string{"-c", "true"}, Cwd: root, SeparateStreams: c.separate,
			})
			// The exit code decides only the error, never the hint.
			if c.res.ExitCode != 0 {
				if err == nil {
					t.Fatalf("a non-zero confined exit must be reported as an error, got none")
				}
			} else if err != nil {
				t.Fatalf("a zero exit must not be an error, got %v", err)
			}

			where := got.Stdout
			if c.separate {
				where = got.Stderr
			}
			hasHint := strings.Contains(where, "[yaah]")
			if hasHint != c.wantHint {
				t.Fatalf("hint present = %v, want %v\nstdout: %q\nstderr: %q",
					hasHint, c.wantHint, got.Stdout, got.Stderr)
			}
			if !c.wantHint {
				return
			}
			if !c.noRoot && !strings.Contains(where, root) {
				t.Errorf("the hint must name the writable root %q, got:\n%s", root, where)
			}
			// The git case carries the deliberate rule, not just the boundary.
			if strings.Contains(c.res.Stdout+c.res.Stderr, ".git/") &&
				!strings.Contains(where, "orchestrator commits") {
				t.Errorf("a git-metadata refusal must state the commit rule, got:\n%s", where)
			}
		})
	}
}
