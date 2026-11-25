package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/pipeline"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// lateHaltProvider wedges its only model call until released, then
// answers normally — the run that completes cleanly so the late-halt
// window can be entered deterministically.
type lateHaltProvider struct {
	release chan struct{}
}

func (p *lateHaltProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	<-p.release
	return &types.ChatResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "predecessor-report"},
			FinishReason: "stop",
		}},
		Usage: types.Usage{TotalTokens: 5},
	}, nil
}

// gateSink parks the dispatch INSIDE runOnce's post-run gauntlet: it
// lets every append through except the per-generation usage fact — the
// first store write made AFTER runOnce captured the stop fact. Parking
// there gives the test a deterministic window to land a supervisor
// halt after the stop fact was captured but before the outer sequence's
// adoption checks read the halt flag (PR #400 review finding 5).
type gateSink struct {
	inner   pipeline.TraceSink
	entered chan struct{}
	proceed chan struct{}
}

func (g *gateSink) Append(ctx context.Context, auth kerneltrace.AppendContext, batch kerneltrace.AppendBatch) (kerneltrace.AppendReceipt, error) {
	for _, grp := range batch.Groups {
		for _, d := range grp.FactDrafts {
			if d.KindLabel == "subagent:usage" {
				g.entered <- struct{}{}
				<-g.proceed
			}
		}
	}
	return g.inner.Append(ctx, auth, batch)
}

func (g *gateSink) PublishFrontier(ctx context.Context, auth kerneltrace.AppendContext, spec kerneltrace.FrontierSpec) (kerneltrace.Frontier, error) {
	return g.inner.PublishFrontier(ctx, auth, spec)
}

func (g *gateSink) Close(ctx context.Context) error { return g.inner.Close(ctx) }

// TestTaskRunner_LateHaltDuringRedirectAdoption pins PR #400 review
// finding 5: a supervisor halt landing after runOnce captured the run's
// stop fact but before the redirect-adoption check must not surface as
// the predecessor's successful result (or its cancellation as an
// ordinary, retryable failure). The topology: the run completes
// normally (stop fact: not halted), the halt flag flips while the
// dispatch is parked after that capture, a redirect claim is pending —
// and the outcome must be the deliberate "halted by supervisor" stop
// with no stale result, and a re-captured stop fact that no longer
// denies the halt.
func TestTaskRunner_LateHaltDuringRedirectAdoption(t *testing.T) {
	initTestRoles(t)

	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	root, err := CreateRootScope(mgr, "sess-latehalt", "", true) // pure-causal root
	if err != nil {
		t.Fatalf("CreateRootScope: %v", err)
	}

	runs := supervision.NewRegistry()
	gate := &gateSink{
		inner:   store,
		entered: make(chan struct{}, 1),
		proceed: make(chan struct{}),
	}
	provider := &lateHaltProvider{release: make(chan struct{})}
	opts := taskRunnerOpts{
		provider:     provider,
		systemPrompt: "BASE",
		modelName:    "test-model",
		subCfg:       config.SubAgentConfig{},
		defaults:     config.Defaults{},
		rootScopeID:  root.ID(),
		runs:         runs,
		scopeManager: mgr,
		traceStore:   gate,
	}
	runner := makeTaskRunner(opts, 0)

	var stop jobs.SubAgentStop
	ctx := jobs.WithSubAgentStop(context.Background(), &stop)

	type runResult struct {
		result string
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, rerr := runner(ctx, "original task", tools.SubAgentParams{Role: "developer"})
		done <- runResult{result, rerr}
	}()

	// Wait for the dispatch to register its scope and start its model call.
	var scopeID string
	deadline := time.After(5 * time.Second)
	for {
		if ids := runs.ScopeIDs(); len(ids) > 0 {
			scopeID = ids[0]
			break
		}
		select {
		case <-deadline:
			t.Fatal("the dispatch never registered its run")
		case <-done:
			t.Fatal("the run ended before it could be halted")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Claim the redirect so the completion adopts: without a claim the
	// sequence would end at !adopted before reaching the halt check.
	gen1, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("the live registration must report its generation")
	}
	if _, err := runs.Redirect(scopeID, gen1, supervisionapi.RedirectRequest{Guidance: "ignored — the halt wins"}); err != nil {
		t.Fatalf("Redirect: %v", err)
	}

	// Let the run complete normally: runOnce returns (result, nil) and
	// captures a stop fact that does NOT know about any halt.
	close(provider.release)

	// Park inside the post-run gauntlet — after CaptureSubAgentStop —
	// then land the halt in that window: the flag flips AFTER the run
	// reported itself not-halted.
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never reached its post-run usage append")
	}
	if _, err := runs.Cancel(scopeID); err != nil { // the halt: marks the flag, cancels gen1
		t.Fatalf("Cancel: %v", err)
	}
	close(gate.proceed)

	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "halted by supervisor") {
			t.Fatalf("late-halted dispatch error = %v (result %q), want the deliberate \"halted by supervisor\" stop", res.err, res.result)
		}
		if res.result != "" {
			t.Errorf("late-halted dispatch result = %q, want no stale predecessor report resurfacing as completion", res.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatch never returned")
	}

	// The stop fact must have been re-captured: it no longer denies the
	// halt (CaptureSubAgentStop writes through the envelope sink).
	if !stop.HaltedBySupervisor {
		t.Error("stop fact HaltedBySupervisor = false, want true after the late-halt re-capture")
	}

	// The sequence ended with the halt: nothing adopted, the bridge is cut.
	if runs.Has(scopeID) {
		t.Error("the registration must be gone after the halted sequence ends")
	}
}
