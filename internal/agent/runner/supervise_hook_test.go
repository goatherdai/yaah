package runner

// supervise_hook_test.go pins the runner half of the headless start seam
// (yaah-0nbb, design §4.8; PR #417 review finding 3): the superviseRun hook
// fires exactly once per dispatch, AFTER the bridge has registered (so a batch
// built at that instant contains this dispatch's scope), never for a dispatch
// the start gate refused (a loop watching a run that does not exist), and its
// returned stop runs at the body's return — after the end-of-sequence envelope
// capture, so the loop's last tick sees the same membership the envelope does.

import (
	"context"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
)

func TestTaskRunner_SuperviseRunHookFiresOnceAfterBridgeAndStopsAfterCapture(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	mgr, store, root := newStartGateFixture(t, "sess-0nbb-hook", nil)
	provider := &recordingInterventionProvider{
		runs: runs,
		recs: []supervisionapi.InterventionRecord{
			interventionRec(1, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		},
	}

	// The envelope sink: the end-of-sequence capture writes here, and the
	// hook's stop must observe it already populated (the defer runs after the
	// capture, which is the ordering the seam's comment promises).
	var captured []tools.SubAgentIntervention
	ctx := tools.WithSubAgentInterventions(tools.WithSupervisedDispatch(context.Background(), true), &captured)

	var calls []string
	var bridgeVisibleInHook bool
	var stopped, capturedAtStop int
	opts := startGateOpts(mgr, store, root.ID(), runs, provider, false)
	opts.superviseRun = func(_ context.Context, scopeID string) func() {
		calls = append(calls, scopeID)
		// The bridge must already be registered when the hook fires: a batch
		// constructed at this instant has to see this dispatch's scope as a
		// member, or the loop's first snapshot would miss the run it exists
		// to supervise.
		bridgeVisibleInHook = len(runs.ScopeIDs()) > 0
		return func() {
			stopped++
			capturedAtStop = len(captured)
		}
	}
	runner := makeTaskRunner(opts, 0)

	if _, err := runner(ctx, "do the work", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("hook calls = %v, want exactly one per dispatch", calls)
	}
	if !bridgeVisibleInHook {
		t.Error("the hook fired before the bridge registered: a batch built there would miss this dispatch's scope")
	}
	if calls[0] == "" || calls[0] == root.ID() {
		t.Errorf("hook scope = %q, want the dispatch's own bridge scope (not empty, not the root)", calls[0])
	}
	if stopped != 1 {
		t.Errorf("stop calls = %d, want exactly 1", stopped)
	}
	if capturedAtStop != 1 {
		t.Errorf("the envelope held %d record(s) when stop ran, want the 1 captured record: stop is deferred to the body's return, AFTER the end-of-sequence capture", capturedAtStop)
	}
}

func TestTaskRunner_SuperviseRunHookNeverFiresForARefusedDispatch(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	// No sandbox: the fixture's scope owns no isolated workspace, so a
	// supervised dispatch under require_reversible is refused at the start
	// gate (the shape TestTaskRunner_SupervisedRequireReversibleRefusesSharedScope pins).
	mgr, store, root := newStartGateFixture(t, "sess-0nbb-hook-refused", nil)
	provider := &countingProvider{}

	hookCalls := 0
	opts := startGateOpts(mgr, store, root.ID(), runs, provider, true)
	opts.superviseRun = func(context.Context, string) func() {
		hookCalls++
		return nil
	}
	runner := makeTaskRunner(opts, 0)

	_, err := runner(tools.WithSupervisedDispatch(context.Background(), true), "fix the widget", tools.SubAgentParams{Role: "developer"})
	if err == nil {
		t.Fatal("a supervised dispatch under require_reversible with no isolated workspace must be refused")
	}
	if hookCalls != 0 {
		t.Errorf("the hook fired %d time(s) for a gate-refused dispatch: a loop would watch a run that never existed", hookCalls)
	}
}
