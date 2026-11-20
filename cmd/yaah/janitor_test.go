package yaah

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
)

// janitorFixture is a real, deregistered-but-aged worktree whose scope is
// terminal and whose proposal is settled — the teardown-missed orphan the
// janitor is allowed to reclaim. Config lives in an isolated YAAH_HOME.
type janitorFixture struct {
	home, repo, wt string
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newJanitorFixture(t *testing.T, ageCfg string) *janitorFixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	root := filepath.Dir(home)
	f := &janitorFixture{home: home, repo: filepath.Join(root, "repo"), wt: filepath.Join(root, "wt-settled")}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAAH_HOME", home)

	if err := os.MkdirAll(f.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "init", "-q")
	gitIn(t, f.repo, "config", "user.email", "t@t")
	gitIn(t, f.repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "add", ".")
	gitIn(t, f.repo, "commit", "-qm", "init")
	gitIn(t, f.repo, "worktree", "add", "--detach", f.wt, "HEAD")

	// Deregister: remove the admin entry, keep the checkout.
	data, err := os.ReadFile(filepath.Join(f.wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: "))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(f.wt, gitdir)
	}
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.repo, "worktree", "prune")

	// Age the directory past any threshold (force makes age irrelevant anyway).
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(f.wt, old, old); err != nil {
		t.Fatal(err)
	}

	// Seed the trace with the records the janitor joins on.
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(home, "trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	append := func(owner, schema, mode string, payload map[string]any) {
		t.Helper()
		m := kerneltrace.Capture
		if mode == "declaration" {
			m = kerneltrace.Declaration
		}
		_, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
			AppendIntentID: fmt.Sprintf("smoke:%s:%s", schema, owner),
			Groups: []kerneltrace.AppendGroup{{
				TraceOwnerID: owner,
				FactDrafts:   []kerneltrace.RecordDraft{{Mode: m, SchemaRef: schema, Payload: payload}},
			}},
		})
		if err != nil {
			t.Fatalf("append %s: %v", schema, err)
		}
	}
	append("owner:parent:main", goatherd.SchemaScopeForked, "declaration", map[string]any{
		"child_owner": "proposal:777:1", "isolated": true,
		"sandbox_backend": "git", "sandbox_path": f.wt,
	})
	append("owner:parent:main", goatherd.SchemaScopeDiscarded, "capture", map[string]any{
		"child_owner": "proposal:777:1", "child_scope": "scope:proposal:777:1",
	})
	append("scope:proposal:777:1", "yaah.proposal.sealed.v1", "capture", map[string]any{"proposal_id": "proposal:777:1"})
	append("scope:proposal:777:1", "yaah.proposal.settled.v1", "capture", map[string]any{"proposal_id": "proposal:777:1"})
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf(`agents:
  default:
    retained_repo_path: %s
    retained_worktree_root: %s
    goatherd_trace:
      dsn: %s
    janitor:
      settled_worktree_age: %s
`, f.repo, root, filepath.Join(home, "trace.db"), ageCfg)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestJanitorRunForceWiring is the CLI-level regression for `--force` having
// silently disabled worktree reclamation: with no age threshold configured,
// a plain run sweeps nothing, but --force reclaims the settled orphan. The
// collector-level tests cover the Force semantics; this pins the flag's
// wiring through janitorRun's option construction.
func TestJanitorRunForceWiring(t *testing.T) {
	f := newJanitorFixture(t, `""`)

	var out bytes.Buffer
	if err := janitorRun(context.Background(), &out, false, false); err != nil {
		t.Fatalf("run (no threshold): %v", err)
	}
	if !strings.Contains(out.String(), "nothing to reclaim") {
		t.Fatalf("run (no threshold) = %q, want nothing to reclaim", out.String())
	}
	if _, err := os.Stat(f.wt); err != nil {
		t.Fatalf("worktree disappeared without force: %v", err)
	}

	out.Reset()
	if err := janitorRun(context.Background(), &out, false, true); err != nil {
		t.Fatalf("run --force: %v", err)
	}
	if !strings.Contains(out.String(), "1 worktree(s)") {
		t.Fatalf("run --force = %q, want one worktree reclaimed", out.String())
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after --force: %v", err)
	}
}

// TestJanitorRunForceStillValidatesConfig: force lifts the age gate, not
// config validation — a negative threshold is still refused even with
// --force, so a typo cannot silently become "reclaim everything".
func TestJanitorRunForceStillValidatesConfig(t *testing.T) {
	f := newJanitorFixture(t, `"-1h"`)
	var out bytes.Buffer
	err := janitorRun(context.Background(), &out, false, true)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("run --force with negative age err = %v, want a negative-config error", err)
	}
	if _, err := os.Stat(f.wt); err != nil {
		t.Errorf("worktree touched despite config error: %v", err)
	}
}
