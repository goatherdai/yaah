package runner

import (
	"context"
	"runtime"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// TestTaskRunner_RegistersRedirectableRunWithFeature pins the registration
// the snapshot reads (yaah-k0x1): a dispatch registers Redirectable, with
// the feature derived from the ORIGINAL dispatch prompt — trimmed,
// whitespace-collapsed — and Reversible stays false while the scope has no
// sandbox to capture (a pure-causal root), which is the honest report.
func TestTaskRunner_RegistersRedirectableRunWithFeature(t *testing.T) {
	initTestRoles(t)

	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	root, err := CreateRootScope(mgr, "sess-flags", "", true) // pure-causal root
	if err != nil {
		t.Fatalf("CreateRootScope: %v", err)
	}

	runs := supervision.NewRegistry()
	opts := taskRunnerOpts{
		provider:     &haltBlockProvider{},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	done := make(chan error, 1)
	go func() {
		_, rerr := runner(context.Background(), "fix the   flaky\nwidget", tools.SubAgentParams{Role: "developer"})
		done <- rerr
	}()

	var reg supervision.Registration
	deadline := time.After(5 * time.Second)
	for {
		if regs := runs.Entries(); len(regs) == 1 {
			reg = regs[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be inspected")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if !reg.Redirectable {
		t.Error("a sub-agent dispatch must register Redirectable")
	}
	if reg.Feature != "fix the flaky widget" {
		t.Errorf("Feature = %q, want the original dispatch prompt collapsed", reg.Feature)
	}
	if reg.Reversible {
		t.Error("a scope with no sandbox has no pre-run capture, so it must not read reversible")
	}

	if _, err := runs.Cancel(reg.ScopeID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the halted run never returned")
	}
}

// TestTaskRunner_ReversibleOnIsolatedSandboxAfterCapture pins the isolated
// case (b) of the reversibility marking (yaah-k0x1, design §4.2): on a
// scope whose fork carries an ISOLATED sandbox — the worktree lane this
// dispatch owns — the pre-run capture succeeds and the registration reads
// reversible, through MarkReversible, the generation-checked write, never
// a registration literal. The fixture's third argument selects the lane:
// false hands the root a sandbox the fork inherits (the fake reports the
// isolated capability, standing in for the real worktree lane); true is
// the isolated session — a pure-causal root with no per-sub-agent
// sandbox, which is case (c) below. The shared-lane case (a) is
// TestTaskRunner_NotReversibleOnSharedSandbox.
func TestTaskRunner_ReversibleOnIsolatedSandboxAfterCapture(t *testing.T) {
	initTestRoles(t)

	mgr, store, root, sb := newRevertFixture(t, "sess-reversible-capture", false)
	if sb == nil {
		t.Skip("no capture-capable sandbox fixture in this environment")
	}

	runs := supervision.NewRegistry()
	opts := taskRunnerOpts{
		provider:     &haltBlockProvider{},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	done := make(chan error, 1)
	go func() {
		_, rerr := runner(context.Background(), "fix the widget", tools.SubAgentParams{Role: "developer"})
		done <- rerr
	}()

	// The mark lands right after the capture, both before the first model
	// call — the wedged provider below holds the run there.
	var reg supervision.Registration
	deadline := time.After(5 * time.Second)
	for {
		if regs := runs.Entries(); len(regs) == 1 && regs[0].Reversible {
			reg = regs[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the captured run never read reversible — MarkReversible never landed")
		case <-done:
			t.Fatal("the run ended before its registration could be inspected")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if _, err := runs.Cancel(reg.ScopeID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the halted run never returned")
	}
}

// wedgeAfterFirstCallProvider signals its channel when the run reaches its
// first model call — by which point the runner has registered the bridge
// and run (or skipped) the pre-run capture — and then wedges until its
// context is cancelled, like haltBlockProvider. The signal is what makes a
// "Reversible stayed false" reading settled rather than raced: everything
// the marking path does has happened by the time Send is entered.
type wedgeAfterFirstCallProvider struct {
	reached chan struct{}
}

func (p *wedgeAfterFirstCallProvider) Send(ctx context.Context, _ types.ChatRequest) (*types.ChatResponse, error) {
	select {
	case p.reached <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestTaskRunner_NotReversibleOnSharedSandbox pins the shared-lane case (a)
// of the reversibility marking (yaah-k0x1, design §4.2): a dispatch whose
// scope shares the session's sandbox is capture-capable — the pre-run
// capture DOES succeed — but a revert could not roll that workspace back
// without removing other writers' changes, so the registration reads
// Reversible false. Marking it reversible would promise a revert that
// silently behaves like a redirect.
func TestTaskRunner_NotReversibleOnSharedSandbox(t *testing.T) {
	initTestRoles(t)

	mgr, store, root, sb := newSharedSandboxFixture(t, "sess-reversible-shared")
	if sb == nil {
		t.Skip("no shared-sandbox fixture in this environment")
	}

	runs := supervision.NewRegistry()
	provider := &wedgeAfterFirstCallProvider{reached: make(chan struct{}, 1)}
	opts := taskRunnerOpts{
		provider:     provider,
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	done := make(chan error, 1)
	go func() {
		_, rerr := runner(context.Background(), "fix the widget", tools.SubAgentParams{Role: "developer"})
		done <- rerr
	}()

	var reg supervision.Registration
	deadline := time.After(5 * time.Second)
	for {
		if regs := runs.Entries(); len(regs) == 1 {
			reg = regs[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be inspected")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The fixture's premise, checked: the dispatch inherited the root's
	// capture-capable sandbox, and that sandbox reports NOT isolated.
	sub, ok := mgr.Get(reg.ScopeID)
	if !ok {
		t.Fatalf("scope %q missing from the manager", reg.ScopeID)
	}
	if sbox := sub.Sandbox(); sbox == nil || sbox.Capabilities().Isolated {
		t.Fatalf("fixture must hand the dispatch a capture-capable SHARED sandbox, got %#v", sbox)
	}

	// Wait until the run reached its first model call: the capture and its
	// mark have both landed by then, so the false reading below is settled,
	// not a snapshot of the instant before the capture ran.
	select {
	case <-provider.reached:
	case <-done:
		t.Fatal("the run ended before its first model call")
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never reached its first model call")
	}

	if regs := runs.Entries(); len(regs) != 1 || regs[0].Reversible {
		t.Errorf("a shared-sandbox dispatch = %+v, want Reversible false despite its successful capture", regs)
	}

	if _, err := runs.Cancel(reg.ScopeID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the halted run never returned")
	}
}

// TestTaskRunner_NotReversibleWithoutSandbox pins the no-substrate case (c)
// of the reversibility marking (yaah-k0x1, design §4.2): a dispatch whose
// scope has no sandbox at all — a pure-causal root, the isolated session
// lane (the fixture's third argument true) — has nothing to capture and
// nothing to roll back, so the capture is skipped and the registration
// reads Reversible false.
func TestTaskRunner_NotReversibleWithoutSandbox(t *testing.T) {
	initTestRoles(t)

	mgr, store, root, sb := newRevertFixture(t, "sess-reversible-nosandbox", true)
	if sb != nil {
		t.Fatal("the isolated fixture must have no sandbox — that is the point")
	}

	runs := supervision.NewRegistry()
	opts := taskRunnerOpts{
		provider:     &haltBlockProvider{},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	done := make(chan error, 1)
	go func() {
		_, rerr := runner(context.Background(), "fix the widget", tools.SubAgentParams{Role: "developer"})
		done <- rerr
	}()

	var reg supervision.Registration
	deadline := time.After(5 * time.Second)
	for {
		if regs := runs.Entries(); len(regs) == 1 {
			reg = regs[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be inspected")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The fixture's premise, checked: the dispatch inherited the
	// pure-causal root — no sandbox at all.
	sub, ok := mgr.Get(reg.ScopeID)
	if !ok {
		t.Fatalf("scope %q missing from the manager", reg.ScopeID)
	}
	if sbox := sub.Sandbox(); sbox != nil {
		t.Fatalf("fixture must give the dispatch no sandbox, got %#v", sbox)
	}

	// Nothing can ever mark this scope — there is no sandbox to capture —
	// so the false reading is settled the moment the registration exists.
	if regs := runs.Entries(); len(regs) != 1 || regs[0].Reversible {
		t.Errorf("a no-sandbox dispatch = %+v, want Reversible false", regs)
	}

	if _, err := runs.Cancel(reg.ScopeID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the halted run never returned")
	}
}

// TestTaskRunner_InFlightObserverFeedsScopeTable pins the feed seam at the
// dispatch level (yaah-k0x1): a real dispatch through makeTaskRunner
// attaches ITS scope's in-flight table to the loop, so the call the fake
// provider requests is visible in the table while it executes and gone
// once the dispatch returns. The NewSubAgentLoop-level proof lives in
// internal/agent's subagent_observer_test.go; this one closes the gap the
// runner wiring sits in.
func TestTaskRunner_InFlightObserverFeedsScopeTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("drives a real bash call, and the bash tool is unavailable on Windows")
	}
	initTestRoles(t)

	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	mgr := goatherd.NewScopeManager(store)
	root, err := CreateRootScope(mgr, "sess-inflight-feed", "", true) // pure-causal root
	if err != nil {
		t.Fatalf("CreateRootScope: %v", err)
	}

	runs := supervision.NewRegistry()
	opts := taskRunnerOpts{
		provider:     &toolCallProvider{cmd: "sleep 1"},
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			// Admit only the sleeper, so the call actually executes and its
			// start and end events bracket a real execution.
			"developer": {AllowCommands: []string{"sleep*"}},
		}},
		defaults:     config.Defaults{},
		guardCheck:   func(ownerID, toolName, argsJSON string) string { return "" },
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   store,
	}
	runner := makeTaskRunner(opts, 0)

	done := make(chan error, 1)
	go func() {
		_, rerr := runner(context.Background(), "pause briefly", tools.SubAgentParams{Role: "developer"})
		done <- rerr
	}()

	// Wait for the dispatch to register its scope under the root.
	var scopeID string
	deadline := time.After(5 * time.Second)
	for {
		if ids := runs.ScopeIDs(); len(ids) == 1 {
			scopeID = ids[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be observed")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The bash call must be visible in the scope's own table while it
	// runs: the loop's start event reached it through the observer the
	// runner attached, and InFlightCalls reads it back through the
	// registry — generation-checked, against THIS run's incarnation.
	gen, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("the live registration must report its generation")
	}
	var saw []supervisionapi.InFlightCall
	observeDeadline := time.After(5 * time.Second)
	for len(saw) == 0 {
		saw = runs.InFlightCalls(scopeID, gen, time.Now())
		if len(saw) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the dispatch finished without an in-flight call ever being visible (err %v) — the observer never fed the scope's table", err)
		case <-observeDeadline:
			t.Fatal("no in-flight call was ever visible — the observer never fed the scope's table")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if saw[0].Name != "bash" {
		t.Errorf("in-flight call = %q, want bash", saw[0].Name)
	}

	// The dispatch returns, and its table drains: the end event removed
	// the call, and MarkReturned cleared anything the closing broker
	// could have lost.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatch never returned")
	}
	if got := runs.InFlightCalls(scopeID, gen, time.Now()); got != nil {
		t.Errorf("InFlightCalls after the dispatch returned = %+v, want nil", got)
	}
}
