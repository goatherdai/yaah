package runner

// supervised_start_test.go pins the runner half of revert by construction
// (design §4.2, yaah-ugtl): a supervised dispatch either owns a rollback
// substrate or it does not start. A failed pre-run capture is fatal, and
// under supervisor.require_reversible a scope with no isolated workspace is
// refused instead of running unrevertable. Unsupervised dispatches keep
// today's warn-and-continue behavior byte for byte.
//
// It also pins the two gaps a review found (yaah-ugtl review M1, M4): the
// gate refusing an unsnapshotted start, and the successor generation of an
// adopted redirect whose OWN capture fails.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
	goatherdsandbox "github.com/goatherdai/goatherd-runtime/sandbox"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/sandboxfake"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// countingProvider answers every model call with a report and counts them, so
// a test can assert that a refused dispatch never reached the loop.
type countingProvider struct{ calls atomic.Int32 }

func (p *countingProvider) Send(context.Context, types.ChatRequest) (*types.ChatResponse, error) {
	p.calls.Add(1)
	return &types.ChatResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}},
		Usage: types.Usage{TotalTokens: 3},
	}, nil
}

// failingCaptureSandbox is the dispatch's rollback substrate with a capture a
// test can turn into a failure. It fails EVERY capture taken while a
// supervision registration is live — a dispatch's own pre-run capture, the one
// supervision makes fatal — and delegates the captures taken while nothing is
// registered, which are the scope-creation baselines (a fork's inherited
// state): those must keep working, or the fork degrades before the gate is
// ever reached.
//
// The predicate is the registration, not a call count (yaah-ugtl review m4):
// counting calls assumed the fork captures exactly once first, so any change
// to the fork's capture schedule would silently move which capture a test
// failed. A test arms fail when it wants the NEXT dispatch capture to fail.
type failingCaptureSandbox struct {
	*sandboxfake.Sandbox
	runs *supervision.Registry
	fail atomic.Bool
}

func newFailingCaptureSandbox(t *testing.T, runs *supervision.Registry) *failingCaptureSandbox {
	t.Helper()
	sb := &failingCaptureSandbox{Sandbox: sandboxfake.New(), runs: runs}
	if err := sb.Create(context.Background(), goatherdsandbox.SandboxSpec{}); err != nil {
		t.Fatalf("create fake sandbox: %v", err)
	}
	return sb
}

func (s *failingCaptureSandbox) Capture(ctx context.Context) (kerneltrace.WorkspaceState, error) {
	if s.fail.Load() && s.runs != nil && len(s.runs.ScopeIDs()) > 0 {
		return kerneltrace.WorkspaceState{}, errors.New("worktree snapshot refused: no space left on device")
	}
	return s.Sandbox.Capture(ctx)
}

// newStartGateFixture builds an in-memory store, a scope manager, and a root
// scope carrying sb (nil for a pure-causal root: the no-isolated-lane case).
func newStartGateFixture(t *testing.T, session string, sb goatherdsandbox.Sandbox) (*goatherd.ScopeManager, *kerneltrace.SQLiteTraceStore, *goatherd.Scope) {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	mgr := goatherd.NewScopeManager(store)
	root, err := mgr.Create(session, sb)
	if err != nil {
		t.Fatalf("create root scope: %v", err)
	}
	return mgr, store, root
}

// startGateOpts is the minimal session posture the gate reads.
func startGateOpts(mgr *goatherd.ScopeManager, store *kerneltrace.SQLiteTraceStore, rootID string, runs *supervision.Registry, provider agent.Provider, requireReversible bool) taskRunnerOpts {
	return taskRunnerOpts{
		provider:          provider,
		systemPrompt:      "BASE",
		modelName:         "test-model",
		subCfg:            config.SubAgentConfig{},
		defaults:          config.Defaults{},
		rootScopeID:       rootID,
		runs:              runs,
		scopeManager:      mgr,
		traceStore:        store,
		requireReversible: requireReversible,
	}
}

// TestTaskRunner_SupervisedCaptureFailureRefusedAtStart pins the first rule:
// a supervised dispatch whose pre-run capture fails returns an error instead
// of logging a warning and running with Reversible false.
func TestTaskRunner_SupervisedCaptureFailureRefusedAtStart(t *testing.T) {
	initTestRoles(t)

	runs := supervision.NewRegistry()
	sb := newFailingCaptureSandbox(t, runs)
	sb.fail.Store(true) // every dispatch capture from here on fails
	mgr, store, root := newStartGateFixture(t, "sess-ugtl-capture", sb)
	provider := &countingProvider{}
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, false), 0)

	_, err := runner(tools.WithSupervisedDispatch(context.Background(), true), "fix the widget", tools.SubAgentParams{Role: "developer"})
	if !errors.Is(err, ErrSupervisedCaptureFailed) {
		t.Fatalf("supervised dispatch with a failed capture = %v, want ErrSupervisedCaptureFailed", err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Errorf("the refused dispatch reached the model %d time(s); it must fail at start", got)
	}
	if ids := runs.ScopeIDs(); len(ids) != 0 {
		t.Errorf("the refused dispatch left a live registration: %v", ids)
	}
}

// TestTaskRunner_UnsupervisedCaptureFailureProceeds pins non-perturbation:
// the identical capture failure outside supervision is today's warning plus
// an unrevertable run, not an error.
func TestTaskRunner_UnsupervisedCaptureFailureProceeds(t *testing.T) {
	initTestRoles(t)

	runs := supervision.NewRegistry()
	sb := newFailingCaptureSandbox(t, runs)
	sb.fail.Store(true)
	mgr, store, root := newStartGateFixture(t, "sess-ugtl-capture-unsup", sb)
	provider := &countingProvider{}
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, true), 0)

	result, err := runner(context.Background(), "fix the widget", tools.SubAgentParams{Role: "developer"})
	if err != nil {
		t.Fatalf("an unsupervised dispatch with a failed capture must still run: %v", err)
	}
	if result != "done" {
		t.Errorf("result = %q, want the sub-agent's report", result)
	}
	if got := provider.calls.Load(); got == 0 {
		t.Error("the unsupervised dispatch never reached the model")
	}
}

// TestTaskRunner_SupervisedRequireReversibleRefusesSharedScope pins the second
// rule: with supervisor.require_reversible on, a scope that owns no isolated
// workspace (a pure-causal root, a container session's shared root) is refused
// with a clear error rather than run unrevertable.
func TestTaskRunner_SupervisedRequireReversibleRefusesSharedScope(t *testing.T) {
	initTestRoles(t)

	mgr, store, root := newStartGateFixture(t, "sess-ugtl-require", nil)
	runs := supervision.NewRegistry()
	provider := &countingProvider{}
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, true), 0)

	_, err := runner(tools.WithSupervisedDispatch(context.Background(), true), "fix the widget", tools.SubAgentParams{Role: "developer"})
	if !errors.Is(err, ErrSupervisedNotReversible) {
		t.Fatalf("supervised require_reversible dispatch on a shared scope = %v, want ErrSupervisedNotReversible", err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Errorf("the refused dispatch reached the model %d time(s); it must fail at start", got)
	}
}

// TestTaskRunner_SupervisedWithoutRequireReversibleRunsUnrevertable pins the
// other half: with the key off the same dispatch proceeds and its snapshot
// reads Reversible false — the honest report, and the pre-yaah-ugtl behavior.
func TestTaskRunner_SupervisedWithoutRequireReversibleRunsUnrevertable(t *testing.T) {
	initTestRoles(t)

	mgr, store, root := newStartGateFixture(t, "sess-ugtl-unrevertable", nil)
	runs := supervision.NewRegistry()
	probe := &reversibleProbeProvider{runs: runs}
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, probe, false), 0)

	if _, err := runner(tools.WithSupervisedDispatch(context.Background(), true), "fix the widget", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("a supervised dispatch without require_reversible must still run: %v", err)
	}
	if !probe.ran {
		t.Fatal("the dispatch never reached the model")
	}
	if probe.reversible {
		t.Error("a scope with no isolated workspace must read Reversible false")
	}
}

// TestSupervisedStartRefusalFailsClosedWithoutACapture pins the gate's reading
// of a dispatch that has a workspace and never attempted the capture (yaah-ugtl
// review m1): "not attempted" is not "succeeded", so the gate refuses instead
// of assuming the better case.
func TestSupervisedStartRefusalFailsClosedWithoutACapture(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	sb := newFailingCaptureSandbox(t, runs)
	mgr, _, root := newStartGateFixture(t, "sess-ugtl-unattempted", sb)
	sub, err := mgr.Fork(root.ID(), "sub-unattempted", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	if gaterr := supervisedStartRefusal(supervisedStart{
		supervised: true,
		role:       subagent.SubAgentRole("developer"),
		subScope:   sub,
		// captureAttempted and captured both false: nothing snapshotted.
	}); !errors.Is(gaterr, ErrSupervisedCaptureSkipped) {
		t.Fatalf("gate verdict = %v, want ErrSupervisedCaptureSkipped for an unsnapshotted start", gaterr)
	}

	// The same posture with the capture attempted AND successful passes: the
	// sandbox is this fake's isolated-reporting one, so no rule objects.
	if gaterr := supervisedStartRefusal(supervisedStart{
		supervised:       true,
		role:             subagent.SubAgentRole("developer"),
		subScope:         sub,
		captureAttempted: true,
		captured:         true,
	}); gaterr != nil {
		t.Errorf("gate verdict = %v, want nil for an attempted, successful capture", gaterr)
	}
}

// TestTaskRunner_RedirectSuccessorCaptureFailureRunsUnrevertable pins the
// successor half of the adoption loop (yaah-ugtl review M4): a redirect claimed
// against a run whose successor's OWN pre-run capture fails is NOT fatal — the
// supervisor's decision already landed, and failing there would destroy the
// handoff — but the successor must read Reversible false, the only honest
// report once its snapshot did not land.
func TestTaskRunner_RedirectSuccessorCaptureFailureRunsUnrevertable(t *testing.T) {
	initTestRoles(t)

	runs := supervision.NewRegistry()
	sb := newFailingCaptureSandbox(t, runs)
	mgr, store, root := newStartGateFixture(t, "sess-ugtl-successor", sb)

	const guidance = "plan B: switch strategy"
	provider := &successorCaptureProvider{
		runs:     runs,
		started:  make(chan struct{}, 1),
		guidance: guidance,
	}
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, false), 0)

	type runResult struct {
		result string
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, rerr := runner(tools.WithSupervisedDispatch(context.Background(), true), "original task", tools.SubAgentParams{Role: "developer"})
		done <- runResult{result, rerr}
	}()

	var scopeID string
	for deadline := time.After(5 * time.Second); ; {
		if ids := runs.ScopeIDs(); len(ids) > 0 {
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
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first model call never started")
	}

	// The fixture must really be revertable at generation 1, or the false
	// asserted below would be vacuous: this dispatch owns an
	// isolated-reporting sandbox and its first capture succeeded.
	if entries := runs.Entries(); len(entries) != 1 || !entries[0].Reversible {
		t.Fatalf("generation 1 must read Reversible true before the successor's capture is made to fail, got %+v", entries)
	}
	// From here the successor's own pre-run capture fails — and only that one:
	// the first generation's capture is already taken.
	sb.fail.Store(true)

	gen1, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("the live registration must report its generation")
	}
	if _, err := runs.Redirect(scopeID, gen1, supervisionapi.RedirectRequest{Guidance: guidance}); err != nil {
		t.Fatalf("Redirect: %v", err)
	}
	if _, err := runs.CancelGeneration(scopeID, gen1); err != nil {
		t.Fatalf("CancelGeneration: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("a successor whose capture failed must still run (the redirect already landed): %v", res.err)
		}
		if res.result != "final-report" {
			t.Errorf("dispatch result = %q, want the successor's report", res.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the redirected dispatch never returned")
	}

	if !provider.sawSuccessor {
		t.Fatal("the successor loop never ran")
	}
	if got := provider.genAt2.Load(); got <= gen1 {
		t.Errorf("successor generation = %d, want a fresh generation after %d", got, gen1)
	}
	if provider.reversibleAt2 {
		t.Error("a successor whose pre-run capture failed must read Reversible false")
	}
}

// successorCaptureProvider wedges its first model call — the run a redirect
// cancels — and answers the call carrying the deposited guidance, recording
// the registration's Reversible flag as the successor loop sees it.
type successorCaptureProvider struct {
	runs     *supervision.Registry
	started  chan struct{} // buffered 1: signals the wedged call began
	guidance string

	sawSuccessor  bool
	reversibleAt2 bool
	genAt2        atomic.Uint64
}

func (p *successorCaptureProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, p.guidance) {
			p.sawSuccessor = true
			if entries := p.runs.Entries(); len(entries) == 1 {
				p.reversibleAt2 = entries[0].Reversible
			}
			if ids := p.runs.ScopeIDs(); len(ids) == 1 {
				if gen, ok := p.runs.Generation(ids[0]); ok {
					p.genAt2.Store(gen)
				}
			}
			return &types.ChatResponse{
				Choices: []types.Choice{{
					Message:      types.Message{Role: "assistant", Content: "final-report"},
					FinishReason: "stop",
				}},
				Usage: types.Usage{TotalTokens: 5},
			}, nil
		}
	}
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// reversibleProbeProvider records the registration's reversibility as the loop
// starts — after the start gate, before the dispatch ends — then answers.
type reversibleProbeProvider struct {
	runs       *supervision.Registry
	ran        bool
	reversible bool
}

func (p *reversibleProbeProvider) Send(context.Context, types.ChatRequest) (*types.ChatResponse, error) {
	p.ran = true
	if regs := p.runs.Entries(); len(regs) == 1 {
		p.reversible = regs[0].Reversible
	}
	return &types.ChatResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		}},
		Usage: types.Usage{TotalTokens: 3},
	}, nil
}
