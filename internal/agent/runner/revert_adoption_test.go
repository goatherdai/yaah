package runner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
	goatherdsandbox "github.com/goatherdai/goatherd-runtime/sandbox"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/sandboxfake"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// revertAdoptionProvider wedges its first model call — the run a revert
// cancels — AFTER writing an artifact into the dispatch's workspace
// (standing in for the cancelled run's debris on disk), and answers the
// call that carries the deposited guidance with a canned report after
// probing whether the artifact survived: the file state the successor
// loop observes IS the rollback assertion (yaah-1050.1.3).
type revertAdoptionProvider struct {
	runs     *supervision.Registry
	sandbox  *sandboxfake.Sandbox // nil for the isolated (no-substrate) fixture
	started  chan struct{}        // buffered 1: signals the wedged call began
	guidance string

	// wroteRun1 records that run 1 mutated the workspace before it was
	// cancelled; fileStateAtRun2 records what the successor observed
	// ("gone", "present", "no-substrate", or "error: …"). Both are
	// written by the dispatch goroutine and read after it returns.
	wroteRun1       bool
	fileStateAtRun2 string
	genAt2          atomic.Uint64
}

func (p *revertAdoptionProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, p.guidance) {
			if ids := p.runs.ScopeIDs(); len(ids) == 1 {
				if gen, ok := p.runs.Generation(ids[0]); ok {
					p.genAt2.Store(gen)
				}
			}
			p.probeRun2(ctx)
			return &types.ChatResponse{
				Choices: []types.Choice{{
					Message:      types.Message{Role: "assistant", Content: "fresh-report"},
					FinishReason: "stop",
				}},
				Usage: types.Usage{TotalTokens: 5},
			}, nil
		}
	}
	// Run 1: leave workspace debris, then wedge until the handoff's
	// cancel arrives.
	if p.sandbox != nil {
		if err := p.sandbox.WriteFile(ctx, "/run1-artifact.txt", []byte("debris from run 1"), 0o644); err != nil {
			return nil, err
		}
		p.wroteRun1 = true
	}
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// probeRun2 records the workspace state the successor loop started on.
func (p *revertAdoptionProvider) probeRun2(ctx context.Context) {
	if p.sandbox == nil {
		p.fileStateAtRun2 = "no-substrate"
		return
	}
	switch _, err := p.sandbox.ReadFile(ctx, "/run1-artifact.txt"); {
	case err == nil:
		p.fileStateAtRun2 = "present"
	case errors.Is(err, fs.ErrNotExist):
		p.fileStateAtRun2 = "gone"
	default:
		p.fileStateAtRun2 = "error: " + err.Error()
	}
}

// revertHandoff builds the dispatch runner over the given root, starts
// it, waits for the first run to register and wedge, performs the
// supervisor's claim → deposit(guidance, revert) → cancel, and returns
// the dispatch outcome. It is the shared skeleton of the revert tests,
// so each test only asserts what makes its case distinct.
func revertHandoff(t *testing.T, mgr *goatherd.ScopeManager, store *kerneltrace.SQLiteTraceStore, rootID string, runs *supervision.Registry, provider *revertAdoptionProvider, revert bool) (string, error) {
	t.Helper()
	opts := taskRunnerOpts{
		provider:     provider,
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  rootID,
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	type runResult struct {
		result string
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, rerr := runner(context.Background(), "original task", tools.SubAgentParams{Role: "developer"})
		done <- runResult{result, rerr}
	}()

	// Wait for the dispatch to register its scope under the root.
	var scopeID string
	deadline := time.After(5 * time.Second)
	for {
		ids := runs.ScopeIDs()
		if len(ids) > 0 {
			scopeID = ids[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be redirected")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Wait until run 1 has written its artifact and is wedged, so the
	// pre-run snapshot provably predates the debris.
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first model call never started")
	}

	// The supervisor's handoff path: claim (CAS), deposit with the
	// revert flag, cancel.
	gen1, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("the live registration must report its generation")
	}
	if _, err := runs.Redirect(scopeID, gen1); err != nil {
		t.Fatalf("Redirect: %v", err)
	}
	if err := runs.DepositRedirect(scopeID, gen1, supervisionapi.RedirectRequest{Guidance: provider.guidance, Revert: revert}); err != nil {
		t.Fatalf("DepositRedirect: %v", err)
	}
	if _, err := runs.Cancel(scopeID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-done:
		return res.result, res.err
	case <-time.After(10 * time.Second):
		t.Fatal("the redirected dispatch never returned")
		return "", nil
	}
}

// newRevertFixture returns an in-memory store, a scope manager, and a
// root scope carrying a created in-memory sandbox — the rollback
// substrate a shared-lane sub-agent scope inherits through the fork.
func newRevertFixture(t *testing.T, session string, isolated bool) (*goatherd.ScopeManager, *kerneltrace.SQLiteTraceStore, *goatherd.Scope, *sandboxfake.Sandbox) {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	mgr := goatherd.NewScopeManager(store)
	if isolated {
		root, err := CreateRootScope(mgr, session, "", true) // pure-causal root
		if err != nil {
			t.Fatalf("CreateRootScope: %v", err)
		}
		return mgr, store, root, nil
	}
	sb := sandboxfake.New()
	if err := sb.Create(context.Background(), goatherdsandbox.SandboxSpec{}); err != nil {
		t.Fatalf("create fake sandbox: %v", err)
	}
	root, err := mgr.Create(session, sb)
	if err != nil {
		t.Fatalf("create root scope with sandbox: %v", err)
	}
	return mgr, store, root, sb
}

// TestTaskRunner_LiveRevertRollsWorkspaceBackBeforeFreshLoop pins
// yaah-1050.1.3 (a)+(b): run 1 writes an artifact, a revert claims the
// handoff, and the adoption loop applies the pre-run snapshot BEFORE
// the fresh loop — the successor observes no artifact — while the
// envelope carries the FRESH run's report.
func TestTaskRunner_LiveRevertRollsWorkspaceBackBeforeFreshLoop(t *testing.T) {
	initTestRoles(t)

	mgr, store, root, sb := newRevertFixture(t, "sess-live-revert", false)
	runs := supervision.NewRegistry()
	const guidance = "plan B: start over from the clean state"
	provider := &revertAdoptionProvider{
		runs:     runs,
		sandbox:  sb,
		started:  make(chan struct{}, 1),
		guidance: guidance,
	}

	result, err := revertHandoff(t, mgr, store, root.ID(), runs, provider, true)
	if err != nil {
		t.Fatalf("reverted dispatch: %v (result %q)", err, result)
	}

	// (b) The fresh loop ran after the rollback and its result is the
	// one the envelope carries.
	if result != "fresh-report" {
		t.Errorf("dispatch result = %q, want the fresh run's report %q", result, "fresh-report")
	}
	if gen := provider.genAt2.Load(); gen == 0 {
		t.Error("the successor loop never registered a fresh generation on the scope")
	}

	// (a) Run 1 really wrote the artifact, and it is gone before the
	// fresh run 2 observed it — the rollback landed between the two.
	if !provider.wroteRun1 {
		t.Fatal("run 1 must write its artifact before being cancelled")
	}
	if provider.fileStateAtRun2 != "gone" {
		t.Errorf("workspace state at the fresh run = %q, want %q — revert must roll back to the pre-run snapshot first", provider.fileStateAtRun2, "gone")
	}
}

// TestTaskRunner_RedirectWithoutRevertLeavesWorkspaceIntact pins the
// Revert=false half of the contract: the identical handoff with the
// flag unset is a plain redirect — the successor adopts the same scope
// and conversation replacement, but the workspace the cancelled run
// left on disk is NOT rolled back.
func TestTaskRunner_RedirectWithoutRevertLeavesWorkspaceIntact(t *testing.T) {
	initTestRoles(t)

	mgr, store, root, sb := newRevertFixture(t, "sess-plain-redir", false)
	runs := supervision.NewRegistry()
	const guidance = "plan B: switch strategy"
	provider := &revertAdoptionProvider{
		runs:     runs,
		sandbox:  sb,
		started:  make(chan struct{}, 1),
		guidance: guidance,
	}

	result, err := revertHandoff(t, mgr, store, root.ID(), runs, provider, false)
	if err != nil {
		t.Fatalf("redirected dispatch: %v (result %q)", err, result)
	}
	if result != "fresh-report" {
		t.Errorf("dispatch result = %q, want the fresh run's report %q", result, "fresh-report")
	}
	if !provider.wroteRun1 {
		t.Fatal("run 1 must write its artifact before being cancelled")
	}
	if provider.fileStateAtRun2 != "present" {
		t.Errorf("workspace state at the fresh run = %q, want %q — Revert=false must leave the workspace as the cancelled run left it", provider.fileStateAtRun2, "present")
	}
}

// TestTaskRunner_LiveRevertOnIsolatedScopeSkipsRollbackWithNote pins
// the nil-sandbox half of yaah-1050.1.3: an isolated session's
// sub-agent scope is pure-causal (no per-sub-agent rollback
// substrate), so a revert skips the rollback — with a clear note — and
// the fresh loop still runs to a normal result. The revert must never
// fail the run it hands off.
func TestTaskRunner_LiveRevertOnIsolatedScopeSkipsRollbackWithNote(t *testing.T) {
	initTestRoles(t)

	// Capture the default logger so the skipped-rollback note is
	// observable, not just emitted (mirrors the fork-degrade test).
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mgr, store, root, sb := newRevertFixture(t, "sess-iso-revert", true)
	if sb != nil {
		t.Fatal("the isolated fixture must have no sandbox — that is the point")
	}
	runs := supervision.NewRegistry()
	const guidance = "plan B: start over"
	provider := &revertAdoptionProvider{
		runs:     runs,
		sandbox:  nil, // isolated scope: nothing to capture or apply
		started:  make(chan struct{}, 1),
		guidance: guidance,
	}

	result, err := revertHandoff(t, mgr, store, root.ID(), runs, provider, true)
	if err != nil {
		t.Fatalf("reverted dispatch on an isolated scope must not fail: %v (result %q)", err, result)
	}
	if result != "fresh-report" {
		t.Errorf("dispatch result = %q, want the fresh run's report %q", result, "fresh-report")
	}
	if provider.fileStateAtRun2 != "no-substrate" {
		t.Errorf("probe = %q, want %q (the fixture runs with no sandbox)", provider.fileStateAtRun2, "no-substrate")
	}
	logged := buf.String()
	if !strings.Contains(logged, "live revert: workspace rollback skipped") {
		t.Errorf("the skipped rollback must surface a clear note, got: %q", logged)
	}
	if !strings.Contains(logged, "no per-sub-agent rollback substrate") {
		t.Errorf("the note must say why the rollback was skipped, got: %q", logged)
	}
}
