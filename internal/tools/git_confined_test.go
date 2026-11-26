package tools

// git_confined_test.go covers the git tool's pre-execution refusal: in a
// confined dispatch worktree the jail admits only the worktree as writable while
// git's index and object store live in the original repository, so a mutating
// action can never land. The tool refuses before spawning git, in the same words
// the exec-time hint uses, instead of spending a round trip on a kernel denial
// (bead yaah-pvm8).
//
// The gate rests entirely on workspace confinement, so the safety property that
// matters most is the negative one: the orchestrator's own workspace must never
// report it. These cases pin both directions.

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// gitTestWorkspace is a workspace double: the embedded Workspace answers the
// path and file methods, while Exec records that the tool tried to run
// something and replays canned output — so "refused before exec" is an
// observable fact rather than an inference from a failure message.
type gitTestWorkspace struct {
	Workspace
	execs int
	out   string
}

func (w *gitTestWorkspace) Exec(context.Context, ExecRequest) (ExecResult, error) {
	w.execs++
	return ExecResult{ExitCode: 0, Stdout: w.out}, nil
}

// confinedGitWorkspace adds the optional Confined signal a jailed dispatch
// worktree carries, so the git tool is handed the workspace a sub-agent gets.
type confinedGitWorkspace struct{ *gitTestWorkspace }

func (w confinedGitWorkspace) Confined() bool { return true }

// TestGitTool_RefusesWritesInConfinedWorktree: mutating actions are refused with
// the one wording before git runs; read-only actions and ordinary workspaces are
// untouched.
func TestGitTool_RefusesWritesInConfinedWorktree(t *testing.T) {
	root := t.TempDir()
	pv := NewPathValidator(root, false, nil)
	pv.WorkDir = root

	// The property the gate's safety rests on: the workspace the orchestrator
	// runs in carries no confiner, so it never reports confinement.
	if WorkspaceConfined(newLocalWorkspace(NewPathValidator(root, false, nil))) {
		t.Fatal("the orchestrator's workspace must never report confinement")
	}

	newConfined := func() (*GitTool, *gitTestWorkspace) {
		dbl := &gitTestWorkspace{Workspace: newLocalWorkspace(pv), out: "ok\n"}
		return &GitTool{WS: confinedGitWorkspace{gitTestWorkspace: dbl}}, dbl
	}
	newOrdinary := func() (*GitTool, *gitTestWorkspace) {
		dbl := &gitTestWorkspace{Workspace: newLocalWorkspace(pv), out: "ok\n"}
		return &GitTool{WS: dbl}, dbl
	}

	t.Run("mutating actions refuse", func(t *testing.T) {
		for _, args := range []string{
			`{"action":"commit","message":"work"}`,
			`{"action":"add","paths":["a.go"]}`,
			`{"action":"push"}`,
			`{"action":"pull"}`,
			// `fetch` changes no ref and no working-tree file, so it needs no
			// approval — and it still cannot run here: it writes the object store
			// and FETCH_HEAD under the common gitdir, outside the jail's writable
			// root. It used to pass this gate, and git then failed with the bare
			// "Permission denied" the gate exists to remove (yaah-pvm8 review, F1).
			`{"action":"fetch"}`,
			// `branch` is refined per argument: creating or deleting a ref
			// writes, listing it does not.
			`{"action":"branch","paths":["new-branch"]}`,
			`{"action":"branch","flags":["-D"],"paths":["dead"]}`,
		} {
			tool, dbl := newConfined()
			out, err := tool.Execute(context.Background(), args)
			if err == nil {
				t.Fatalf("%s: a mutating action must be refused in a confined worktree, got %q", args, out)
			}
			msg := err.Error()
			for _, want := range []string{"[yaah]", root, "orchestrator commits", "cannot run here"} {
				if !strings.Contains(msg, want) {
					t.Errorf("%s: refusal must state %q, got:\n%s", args, want, msg)
				}
			}
			// The tool surface declines before git runs, so it must not report a
			// refusal the sandbox never made: a parent settling the dispatch reads
			// the two as different evidence (yaah-pvm8 review, F3).
			if !strings.Contains(msg, "before running git") {
				t.Errorf("%s: the refusal must say git never ran, got:\n%s", args, msg)
			}
			if strings.Contains(msg, "refused by the sub-agent sandbox") {
				t.Errorf("%s: nothing was refused by the sandbox on this surface, got:\n%s", args, msg)
			}
			if dbl.execs != 0 {
				t.Errorf("%s: the refusal must happen BEFORE exec, exec ran %d time(s)", args, dbl.execs)
			}
		}
	})

	t.Run("read-only actions still exec", func(t *testing.T) {
		for _, args := range []string{
			`{"action":"status"}`,
			`{"action":"diff"}`,
			`{"action":"log"}`,
			`{"action":"branch"}`,
			`{"action":"branch","flags":["-a"]}`,
		} {
			tool, dbl := newConfined()
			out, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("%s: a read-only action must not be gated, got error %v", args, err)
			}
			if dbl.execs != 1 {
				t.Fatalf("%s: exec ran %d times, want 1", args, dbl.execs)
			}
			if out != "ok" {
				t.Errorf("%s: output %q, want the exec result", args, out)
			}
		}
	})

	t.Run("ordinary workspace still execs a write", func(t *testing.T) {
		tool, dbl := newOrdinary()
		out, err := tool.Execute(context.Background(), `{"action":"commit","message":"work"}`)
		if err != nil {
			t.Fatalf("the orchestrator's own commit must not be gated, got %v", err)
		}
		if dbl.execs != 1 {
			t.Fatalf("exec ran %d times, want 1", dbl.execs)
		}
		if out != "ok" {
			t.Errorf("output %q, want the exec result", out)
		}
	})
}

// TestGitTool_ApprovalAndConfinementAreSeparateQuestions pins the two columns of
// the action table the gate rests on. They are allowed to diverge, and `fetch` is
// where they do: it needs no approval (it changes no ref and no working-tree
// file) and still cannot land in a dispatch worktree (it writes the object store
// and FETCH_HEAD under the original repository's gitdir). The first cut derived
// the write question from the approval bit, so `git fetch` passed the gate and
// git then failed with exactly the bare "Permission denied" this gate exists to
// remove (bead yaah-pvm8 review, F1).
func TestGitTool_ApprovalAndConfinementAreSeparateQuestions(t *testing.T) {
	writers := make([]string, 0, len(allowedGitActions))
	approval := make([]string, 0, len(allowedGitActions))
	for action, info := range allowedGitActions {
		if info.writes {
			writers = append(writers, action)
		}
		if info.dangerous {
			approval = append(approval, action)
		}
	}
	slices.Sort(writers)
	slices.Sort(approval)

	if got, want := strings.Join(writers, ","), "add,branch,commit,fetch,pull,push"; got != want {
		t.Errorf("actions that write git metadata = %s, want %s", got, want)
	}
	if got, want := strings.Join(approval, ","), "add,commit,pull,push"; got != want {
		t.Errorf("actions that need approval = %s, want %s", got, want)
	}

	// The observable consequences, stated through the two predicates themselves.
	if (&GitTool{}).IsDangerous(`{"action":"fetch"}`) {
		t.Error("fetch needs no approval; approval must not be derived from the write column")
	}
	if !gitActionWrites("fetch", nil, nil) {
		t.Error("fetch writes the object store and FETCH_HEAD, so the write jail must refuse it")
	}
	if !(&GitTool{}).IsDangerous(`{"action":"commit","message":"work"}`) {
		t.Error("commit needs approval; the approval column must stay unchanged")
	}
	if gitActionWrites("status", nil, nil) || gitActionWrites("log", nil, nil) {
		t.Error("reading actions write nothing and must not be gated")
	}
	if !gitActionWrites("branch", []string{"-D"}, []string{"dead"}) ||
		gitActionWrites("branch", nil, nil) {
		t.Error("branch must stay refined per argument: -D writes, bare branch lists")
	}

	// The tool surface names the writers it can actually issue, from that table:
	// the shell surface's fixed list (stash, checkout, reset) would name actions
	// this tool rejects as unsupported.
	if got, want := gitWriteActionList(),
		"add, commit, fetch, pull, push, and the branch forms that create or delete a ref"; got != want {
		t.Errorf("gitWriteActionList() = %q, want %q", got, want)
	}
}
