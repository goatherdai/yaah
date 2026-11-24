package runner

import (
	"context"
	"strings"
	"sync/atomic"
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

// redirectAdoptionProvider wedges its first model call — the run a
// redirect cancels — and answers the first call that carries the
// deposited guidance (the successor loop's first user message) with a
// canned report. It records the successor's messages and the registry
// generation live when the successor ran, so the test can pin the
// adoption itself, not just the final envelope.
type redirectAdoptionProvider struct {
	runs     *supervision.Registry
	started  chan struct{} // buffered 1: signals the wedged call began
	genAt2   atomic.Uint64
	msgs2    []types.Message
	guidance string
}

func (p *redirectAdoptionProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, p.guidance) {
			p.msgs2 = req.Messages
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

// TestTaskRunner_RedirectAdoptionRunsFreshLoopOnSameScope pins the
// yaah-8gxg.4 adoption loop end to end: a redirect claims the live
// generation, deposits guidance, and cancels the current run; when that
// run returns, the outer sequence consumes the request, opens a fresh
// generation on the SAME scope with a FRESH steerCh (the predecessor's
// buffered guidance must not leak — finding #6), and runs a fresh loop
// whose first user message is the guidance. The envelope carries the
// FINAL run's report (finding #1), never the cancelled original's.
func TestTaskRunner_RedirectAdoptionRunsFreshLoopOnSameScope(t *testing.T) {
	initTestRoles(t)

	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	root, err := CreateRootScope(mgr, "sess-redir", "", true) // pure-causal root
	if err != nil {
		t.Fatalf("CreateRootScope: %v", err)
	}

	runs := supervision.NewRegistry()
	const guidance = "plan B: switch strategy"
	provider := &redirectAdoptionProvider{
		runs:     runs,
		started:  make(chan struct{}, 1),
		guidance: guidance,
	}
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

	// Wait until the first model call is in flight, so injected guidance
	// lands in the FIRST generation's steering buffer undrained.
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first model call never started")
	}
	if err := runs.Inject(scopeID, "stale-guidance-must-not-leak"); err != nil {
		t.Fatalf("Inject into run 1: %v", err)
	}

	// The supervisor's redirect path: claim publishes the complete
	// payload atomically (PR #398 review finding), then cancel.
	gen1, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("the live registration must report its generation")
	}
	if _, err := runs.Redirect(scopeID, gen1, supervisionapi.RedirectRequest{Guidance: guidance}); err != nil {
		t.Fatalf("Redirect: %v", err)
	}
	// The redirect's generation-checked stop runs through Handoff —
	// plain cancellation, never the halt mark (finding #4).
	if _, err := runs.CancelGeneration(scopeID, gen1); err != nil {
		t.Fatalf("CancelGeneration: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("redirected dispatch: %v (result %q)", res.err, res.result)
		}
		if res.result != "final-report" {
			t.Errorf("dispatch result = %q, want the FINAL run's report %q (finding #1)", res.result, "final-report")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the redirected dispatch never returned")
	}

	// The successor ran on the SAME scope under a NEW generation: the
	// bridge was re-registered before the successor's loop ran.
	if got := provider.genAt2.Load(); got <= gen1 {
		t.Errorf("successor generation = %d, want a fresh generation after %d (re-Register on the same scope)", got, gen1)
	}

	// The successor's conversation leads with the deposited guidance as
	// its first user message...
	if len(provider.msgs2) == 0 {
		t.Fatal("the successor loop saw no messages")
	}
	var firstUser string
	for _, m := range provider.msgs2 {
		if m.Role == "user" {
			firstUser = m.Content
			break
		}
	}
	if !strings.Contains(firstUser, guidance) {
		t.Errorf("successor's first user message = %q, want the deposited guidance", firstUser)
	}

	// ...and its steerCh is FRESH: the stale guidance buffered into the
	// predecessor's channel never reaches the successor's conversation
	// (finding #6).
	for _, m := range provider.msgs2 {
		if strings.Contains(m.Content, "stale-guidance") {
			t.Errorf("stale guidance leaked into the successor's messages: %q", m.Content)
		}
	}

	// The sequence's teardown: the bridge is gone once the final run's
	// result has been returned.
	if runs.Has(scopeID) {
		t.Error("the registration must be gone after the sequence ends")
	}
}
