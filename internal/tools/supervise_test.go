package tools

// supervise_test.go covers yaah-fy28's acceptance at the tool boundary: the
// binding (descendants only, the root never a target, one batch per calling
// loop), the TickResult -> reply mapping for both wait modes, the arbiter
// lifetime the tool owns (arm on bind, release on Done, re-arm on reuse), the
// busy path, and a tool-level end-to-end against the REAL registry, the REAL
// scope tree and the REAL Batch (a runner-level dispatch would additionally
// need the parallel bead's runner/delegate changes, so the fake run bridges
// here stand in for workers: their steering channels and handoff callbacks are
// what an intervention actually reaches).

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

type superviseFixture struct {
	t     *testing.T
	store *kerneltrace.SQLiteTraceStore
	mgr   *goatherd.ScopeManager
	runs  *supervision.Registry
	wait  func(ctx context.Context, d time.Duration) error
}

func newSuperviseFixture(t *testing.T) *superviseFixture {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return &superviseFixture{
		t:     t,
		store: store,
		mgr:   goatherd.NewScopeManager(store),
		runs:  supervision.NewRegistry(),
		// An instant tick: the batch's wait is the only sleep, and injected it
		// makes every test here deterministic.
		wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
}

func (f *superviseFixture) root(owner string) string {
	f.t.Helper()
	scope, err := f.mgr.Create(owner, nil)
	if err != nil {
		f.t.Fatalf("create scope %s: %v", owner, err)
	}
	return scope.ID()
}

func (f *superviseFixture) fork(parent, owner string) string {
	f.t.Helper()
	scope, err := f.mgr.Fork(parent, owner, nil)
	if err != nil {
		f.t.Fatalf("fork %s under %s: %v", owner, parent, err)
	}
	return scope.ID()
}

// arbiterHooks counts the binding's arm/release calls: the tool owns when the
// rules engine is routed into the batch and when it goes back to direct
// delivery, and these numbers are the only way to see it from outside.
type arbiterHooks struct {
	arms     int
	releases int
}

// tool builds the tool the composition root builds: the same snapshot source
// chain, the same descendant filter, the same batch config (no-op wait, the
// documented one-tick cooldown, production's superviseBatchConfig selector).
func (f *superviseFixture) tool(caller string) *SuperviseTool {
	return f.toolWithHooks(caller, nil)
}

func (f *superviseFixture) toolWithHooks(caller string, hooks *arbiterHooks) *SuperviseTool {
	src := &supervision.Snapshotter{Runs: f.runs, Store: f.store}
	return &SuperviseTool{
		CallerScopeID: caller,
		NewBatch: func(callerScopeID string) *SuperviseBinding {
			binding := &SuperviseBinding{
				Batch: supervision.NewBatch(f.runs,
					&DescendantSnapshot{Source: src, Scopes: f.mgr, Caller: callerScopeID},
					supervision.BatchConfig{
						RootScopeID:   callerScopeID,
						CooldownTicks: -1, // the documented default: one tick
						Wait:          f.wait,
					}),
			}
			if hooks != nil {
				binding.Arm = func() { hooks.arms++ }
				binding.Release = func() { hooks.releases++ }
			}
			return binding
		},
	}
}

func steerRun() supervision.Run {
	return supervision.Run{SteerCh: make(chan string, supervision.SteerBuffer)}
}

// handoffRun is a redirectable run whose handoff callback counts deliveries:
// the count is how "exactly once per accepted decision" is asserted.
func handoffRun() (supervision.Run, *int) {
	calls := 0
	return supervision.Run{
		SteerCh:      make(chan string, supervision.SteerBuffer),
		Redirectable: true,
		Handoff:      func() { calls++ },
	}, &calls
}

func executeSupervise(t *testing.T, tool *SuperviseTool, args string) (superviseResult, error) {
	t.Helper()
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		return superviseResult{}, err
	}
	var res superviseResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("reply is not the documented JSON shape: %v\n%s", err, out)
	}
	return res, nil
}

// stubBatch is the tool's SuperviseBatch contract for the two cases the real
// batch cannot produce on demand: a refused tick, and a tick held open across a
// second call.
type stubBatch struct {
	next  func(ctx context.Context, decisions []supervisionapi.Decision) (supervisionapi.TickResult, error)
	text  string
	calls int
}

func (s *stubBatch) Next(ctx context.Context, decisions []supervisionapi.Decision) (supervisionapi.TickResult, error) {
	s.calls++
	return s.next(ctx, decisions)
}

func (s *stubBatch) SnapshotText(int) string { return s.text }

func stubTool(batch SuperviseBatch) *SuperviseTool {
	return &SuperviseTool{
		CallerScopeID: "scope:root",
		NewBatch: func(string) *SuperviseBinding {
			return &SuperviseBinding{Batch: batch}
		},
	}
}

// TestSuperviseTool_BindsToCallingScopesDescendants pins the membership
// boundary: only the calling scope's strict descendants are in the batch, a
// sibling subtree's worker is not addressable, and the root is never a target.
func TestSuperviseTool_BindsToCallingScopesDescendants(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	siblingRoot := f.root("other") // a second top-level scope: not a descendant
	worker := f.fork(root, "sub-a")
	otherWorker := f.fork(siblingRoot, "sub-o")

	workerCh := make(chan string, supervision.SteerBuffer)
	f.runs.Register(root, supervision.Run{SteerCh: make(chan string, supervision.SteerBuffer)})
	f.runs.Register(siblingRoot, supervision.Run{SteerCh: make(chan string, supervision.SteerBuffer)})
	f.runs.Register(worker, supervision.Run{SteerCh: workerCh})
	f.runs.Register(otherWorker, steerRun())

	tool := f.tool(root)
	decisions, err := json.Marshal(map[string]any{"decisions": []map[string]string{
		{"scope": worker, "action": "steer", "reason": "drift", "guidance": "use the helper"},
		{"scope": otherWorker, "action": "steer", "reason": "drift", "guidance": "not yours"},
		{"scope": root, "action": "steer", "reason": "self", "guidance": "not a target"},
	}})
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	res, err := executeSupervise(t, tool, string(decisions))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(res.Applied) != 1 || res.Applied[0].Scope != worker || res.Applied[0].Action != "steer" || res.Applied[0].Outcome != "applied" {
		t.Fatalf("applied = %+v, want exactly the descendant's steer applied", res.Applied)
	}
	got := <-workerCh
	if got != "use the helper" {
		t.Errorf("worker received %q, want the decision's guidance", got)
	}

	reasons := map[string]string{}
	for _, rec := range res.Results {
		if rec.DropReason != "" {
			reasons[rec.Scope] = rec.DropReason
		}
	}
	if reasons[otherWorker] != "stale" {
		t.Errorf("sibling worker's drop reason = %q, want stale — it must not even resolve inside this batch", reasons[otherWorker])
	}
	if reasons[root] != "root_scope" {
		t.Errorf("root's drop reason = %q, want root_scope", reasons[root])
	}
	if strings.Contains(res.Snapshot, otherWorker) {
		t.Errorf("rendered snapshot names %s, which is not a descendant of %s:\n%s", otherWorker, root, res.Snapshot)
	}
}

// TestSuperviseTool_SecondCallerRefused pins "one batch per calling loop".
func TestSuperviseTool_SecondCallerRefused(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	other := f.root("other")
	f.runs.Register(f.fork(root, "sub-a"), steerRun())

	tool := f.tool(root)
	if _, err := executeSupervise(t, tool, `{}`); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	tool.CallerScope = func(context.Context) string { return other }
	_, err := executeSupervise(t, tool, `{}`)
	if err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("second caller err = %v, want the one-batch-per-loop refusal", err)
	}
}

// TestSuperviseTool_StrictArgs pins strict key handling at both levels.
func TestSuperviseTool_StrictArgs(t *testing.T) {
	tool := &SuperviseTool{CallerScopeID: "scope:root"}
	for _, tc := range []struct{ name, args string }{
		{"unknown top-level key", `{"decisons": []}`},
		{"unknown key inside a decision", `{"decisions":[{"scope":"w1","action":"steer","guidence":"x"}]}`},
		{"invalid wait mode", `{"wait":"later"}`},
	} {
		if _, err := tool.Execute(context.Background(), tc.args); err == nil {
			t.Errorf("%s: err = nil, want a refusal", tc.name)
		}
	}
}

// TestSuperviseTool_StopActionDroppedAsUnknownAction pins that the rules-only
// verb reaching the policy list is REPORTED, not silently ignored: stop is not
// in the decision vocabulary, so it is dropped with unknown_action — the one
// drop reason that fires before the scope is even resolved.
func TestSuperviseTool_StopActionDroppedAsUnknownAction(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	worker := f.fork(root, "sub-a")
	ch := make(chan string, supervision.SteerBuffer)
	f.runs.Register(worker, supervision.Run{SteerCh: ch})

	tool := f.tool(root)
	res, err := executeSupervise(t, tool, `{"decisions":[{"scope":"`+worker+`","action":"stop"}]}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("applied = %+v, want nothing: stop is the rules engine's verb", res.Applied)
	}
	var drop string
	for _, rec := range res.Results {
		if rec.Scope == worker {
			drop = rec.DropReason
			if rec.Action != "stop" {
				t.Errorf("record action = %q, want the submitted %q", rec.Action, "stop")
			}
		}
	}
	if drop != supervisionapi.DropReasonUnknownAction {
		t.Errorf("drop reason = %q, want %q", drop, supervisionapi.DropReasonUnknownAction)
	}
	if len(ch) != 0 {
		t.Errorf("a steer reached the worker %d time(s) from a stop decision", len(ch))
	}
}

// TestSuperviseTool_SteerRedirectRevertExactlyOnce is the end-to-end: through
// the tool, one tick accepts four decisions and delivers each accepted action
// exactly once, reporting the one that could not be taken; the next tick's
// identical list is cooled out, so nothing is delivered twice.
func TestSuperviseTool_SteerRedirectRevertExactlyOnce(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	steered := f.fork(root, "sub-steer")
	redirected := f.fork(root, "sub-redirect")
	reverted := f.fork(root, "sub-revert")
	notReversible := f.fork(root, "sub-no-capture")

	steerCh := make(chan string, supervision.SteerBuffer)
	f.runs.Register(steered, supervision.Run{SteerCh: steerCh})
	redirectRun, redirectCalls := handoffRun()
	f.runs.Register(redirected, redirectRun)
	revertRun, revertCalls := handoffRun()
	f.runs.Register(reverted, revertRun)
	plainRun, plainCalls := handoffRun()
	f.runs.Register(notReversible, plainRun)

	// Reversible is marked post-capture, generation-checked — the same call the
	// runner makes once its pre-run snapshot has landed.
	gen, ok := f.runs.Generation(reverted)
	if !ok {
		t.Fatal("reverted worker has no live generation")
	}
	if !f.runs.MarkReversible(reverted, gen) {
		t.Fatal("MarkReversible did not land")
	}

	args := `{"decisions":[
		{"scope":"` + steered + `","action":"steer","reason":"drift","guidance":"use the helper"},
		{"scope":"` + redirected + `","action":"redirect","reason":"loop","guidance":"start over with the spec"},
		{"scope":"` + reverted + `","action":"revert","reason":"corrupt tree","guidance":"re-read the file you truncated"},
		{"scope":"` + notReversible + `","action":"revert","reason":"corrupt tree","guidance":"roll back"}]}`

	tool := f.tool(root)
	res, err := executeSupervise(t, tool, args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := map[string]string{steered: "applied", redirected: "claimed", reverted: "claimed"}
	if len(res.Applied) != len(want) {
		t.Fatalf("applied = %+v, want the three accepted actions", res.Applied)
	}
	for _, a := range res.Applied {
		if want[a.Scope] != a.Outcome {
			t.Errorf("%s outcome = %q, want %q", a.Scope, a.Outcome, want[a.Scope])
		}
	}
	var drop string
	for _, rec := range res.Results {
		if rec.Scope == notReversible {
			drop = rec.DropReason
		}
	}
	if drop != "not_reversible" {
		t.Errorf("non-reversible revert drop reason = %q, want not_reversible (never downgraded)", drop)
	}

	if got := len(steerCh); got != 1 {
		t.Errorf("steer delivered %d times, want 1", got)
	}
	if *redirectCalls != 1 || *revertCalls != 1 {
		t.Errorf("handoffs = %d/%d, want 1/1 (once per accepted decision)", *redirectCalls, *revertCalls)
	}
	if *plainCalls != 0 {
		t.Errorf("non-reversible scope's handoff ran %d times, want 0", *plainCalls)
	}
	// The revert's revert flag reaches the adoption payload.
	pending, ok := f.runs.ConsumeRedirectOrClose(reverted, gen)
	if !ok {
		t.Fatal("no pending redirect payload for the reverted scope")
	}
	if !pending.Revert {
		t.Error("revert decision deposited a redirect without the revert flag")
	}

	// The identical list on the next tick: cooled out, nothing re-delivered.
	again, err := executeSupervise(t, tool, args)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if len(again.Applied) != 0 {
		t.Fatalf("second tick applied = %+v, want nothing (cooldown)", again.Applied)
	}
	if len(steerCh) != 1 || *redirectCalls != 1 || *revertCalls != 1 {
		t.Error("second tick re-delivered an intervention the cooldown should have suppressed")
	}
}

// TestSuperviseTool_WaitSettledReturnsRecords pins wait:"settled": the call
// ticks until every scope has finished and returns the intervention records.
func TestSuperviseTool_WaitSettledReturnsRecords(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	worker := f.fork(root, "sub-a")
	ch := make(chan string, supervision.SteerBuffer)
	f.runs.Register(worker, supervision.Run{SteerCh: ch})

	// The worker finishes during the first tick's wait — the shape of a
	// dispatch that ends inside the interval.
	settled := false
	f.wait = func(ctx context.Context, _ time.Duration) error {
		if !settled {
			settled = true
			f.runs.Unregister(worker)
		}
		return ctx.Err()
	}

	tool := f.tool(root)
	out, err := tool.Execute(context.Background(),
		`{"wait":"settled","decisions":[{"scope":"`+worker+`","action":"steer","reason":"drift","guidance":"wrap up"}]}`)
	if err != nil {
		t.Fatalf("Execute(settled): %v", err)
	}
	var wire struct {
		Done     *bool            `json:"done"`
		Busy     *bool            `json:"busy"`
		Snapshot string           `json:"snapshot"`
		Applied  []map[string]any `json:"applied"`
		Results  []map[string]any `json:"results"`
		Ticks    int              `json:"ticks"`
	}
	if err := json.Unmarshal([]byte(out), &wire); err != nil {
		t.Fatalf("reply is not JSON: %v\n%s", err, out)
	}
	if wire.Done == nil || !*wire.Done {
		t.Fatalf("done = %v, want true once every scope finished\n%s", wire.Done, out)
	}
	if wire.Busy == nil || *wire.Busy {
		t.Fatalf("busy = %v, want false\n%s", wire.Busy, out)
	}
	if wire.Ticks != 1 {
		t.Errorf("ticks = %d, want 1 (the decisions tick was the settling tick)", wire.Ticks)
	}
	if len(wire.Applied) != 1 {
		t.Fatalf("applied = %+v, want the steer", wire.Applied)
	}
	for _, key := range []string{"scope", "action", "outcome"} {
		if _, ok := wire.Applied[0][key]; !ok {
			t.Errorf("applied entry missing %q: %+v", key, wire.Applied[0])
		}
	}
	if len(wire.Results) != 1 {
		t.Fatalf("results = %+v, want the intervention record", wire.Results)
	}
	if wire.Results[0]["reason"] != "drift" || wire.Results[0]["outcome"] != "applied" {
		t.Errorf("record = %+v, want the decision's reason and outcome", wire.Results[0])
	}
	if len(ch) != 1 {
		t.Errorf("steer delivered %d times, want 1", len(ch))
	}
}

// TestSuperviseTool_SettledSpansMultipleTicks pins the accumulation loop: a
// batch that takes several ticks to settle reports every tick it ran, not just
// the last one.
func TestSuperviseTool_SettledSpansMultipleTicks(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	worker := f.fork(root, "sub-a")
	f.runs.Register(worker, steerRun())

	waits := 0
	f.wait = func(ctx context.Context, _ time.Duration) error {
		waits++
		if waits >= 3 {
			f.runs.Unregister(worker)
		}
		return ctx.Err()
	}

	tool := f.tool(root)
	res, err := executeSupervise(t, tool, `{"wait":"settled"}`)
	if err != nil {
		t.Fatalf("Execute(settled): %v", err)
	}
	if !res.Done {
		t.Fatal("done = false, want true: the worker finished on the third wait")
	}
	if res.Ticks != 3 {
		t.Errorf("ticks = %d, want 3 (two ticks saw a live worker, the third saw it finish)", res.Ticks)
	}
	if len(res.Applied) != 0 || len(res.Results) != 0 {
		t.Errorf("applied/results = %+v/%+v, want nothing: the call supplied no decisions", res.Applied, res.Results)
	}
}

// TestSuperviseTool_SettledWithNoLiveDescendants pins the honest empty reply:
// with nothing to supervise the batch is vacuously Done, so a settled call
// returns immediately instead of hanging on a universe that never had a worker.
func TestSuperviseTool_SettledWithNoLiveDescendants(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")

	tool := f.tool(root)
	res, err := executeSupervise(t, tool, `{"wait":"settled"}`)
	if err != nil {
		t.Fatalf("Execute(settled): %v", err)
	}
	if !res.Done {
		t.Error("done = false, want true: no live descendant is vacuously settled")
	}
	if res.Ticks != 1 {
		t.Errorf("ticks = %d, want 1", res.Ticks)
	}
	if len(res.Applied) != 0 || len(res.Results) != 0 {
		t.Errorf("applied/results = %+v/%+v, want empty", res.Applied, res.Results)
	}
	// The footer's example decision always names w1, so the honest check is the
	// header's own count: nothing may be reported as supervised.
	if !strings.Contains(res.Snapshot, "0 agents") {
		t.Errorf("snapshot does not report an empty membership:\n%s", res.Snapshot)
	}
}

// TestSuperviseTool_ReleasesArbiterWhenDoneAndRearms pins the arbiter lifetime:
// the rules engine is routed into the batch while the loop ticks, RELEASED back
// to direct delivery the moment a tick reports Done (so later rule firings are
// delivered rather than queued into a batch nothing drives), and re-armed when a
// new dispatch makes the same batch live again.
func TestSuperviseTool_ReleasesArbiterWhenDoneAndRearms(t *testing.T) {
	f := newSuperviseFixture(t)
	root := f.root("root")
	worker := f.fork(root, "sub-a")
	f.runs.Register(worker, steerRun())
	hooks := &arbiterHooks{}

	tool := f.toolWithHooks(root, hooks)
	res, err := executeSupervise(t, tool, `{}`)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if res.Done {
		t.Fatal("done = true, want false: a live worker remains")
	}
	if hooks.arms != 1 || hooks.releases != 0 {
		t.Fatalf("arms/releases = %d/%d, want 1/0: binding must route the rules engine into the batch", hooks.arms, hooks.releases)
	}

	// The worker finishes: the next tick settles the batch and releases.
	f.runs.Unregister(worker)
	res, err = executeSupervise(t, tool, `{}`)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if !res.Done {
		t.Fatalf("done = false, want true once the only worker finished\n%s", res.Snapshot)
	}
	if hooks.releases != 1 {
		t.Fatalf("releases = %d, want 1: a settled batch must hand the rules engine back to direct delivery", hooks.releases)
	}

	// A new dispatch appears: the same batch is live again, so the next call
	// re-arms the arbiter instead of ticking a batch the rules no longer reach.
	f.runs.Register(f.fork(root, "sub-b"), steerRun())
	res, err = executeSupervise(t, tool, `{}`)
	if err != nil {
		t.Fatalf("third Execute: %v", err)
	}
	if res.Done {
		t.Error("done = true, want false: a new dispatch is live")
	}
	if hooks.arms != 2 {
		t.Errorf("arms = %d, want 2 (re-armed after release)", hooks.arms)
	}
	if hooks.releases != 1 {
		t.Errorf("releases = %d, want 1: nothing settled on the third call", hooks.releases)
	}
}

// TestSuperviseTool_BatchBusyIsAStructuredReply pins the busy path's shape: a
// refused tick is a scheduling fact with nothing applied, reported as data, not
// as a failed turn.
func TestSuperviseTool_BatchBusyIsAStructuredReply(t *testing.T) {
	batch := &stubBatch{
		next: func(context.Context, []supervisionapi.Decision) (supervisionapi.TickResult, error) {
			return supervisionapi.TickResult{}, supervisionapi.ErrBatchBusy
		},
		text: "w1 ...",
	}
	tool := stubTool(batch)

	out, err := tool.Execute(context.Background(), `{"decisions":[{"scope":"w1","action":"steer","reason":"drift","guidance":"x"}]}`)
	if err != nil {
		t.Fatalf("Execute: %v, want the busy reply rather than an error", err)
	}
	var res superviseResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("reply is not JSON: %v\n%s", err, out)
	}
	if !res.Busy {
		t.Errorf("busy = false, want true\n%s", out)
	}
	if res.Ticks != 0 || len(res.Applied) != 0 || len(res.Results) != 0 {
		t.Errorf("ticks/applied/results = %d/%+v/%+v, want 0/nothing/nothing", res.Ticks, res.Applied, res.Results)
	}
	if res.Note == "" {
		t.Error("note is empty: the busy reply must say what to do next")
	}
}

// TestSuperviseTool_ConcurrentCallGetsBusyReply pins the single flight: the
// render is inside it (label stability), so a second call on the same session
// while one is in flight is refused rather than allowed to render, and it is
// refused immediately rather than queued behind a tick that can block for a
// whole interval or, on a settle, for the batch's life.
func TestSuperviseTool_ConcurrentCallGetsBusyReply(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	batch := &stubBatch{
		next: func(context.Context, []supervisionapi.Decision) (supervisionapi.TickResult, error) {
			once.Do(func() { close(entered) })
			<-release
			return supervisionapi.TickResult{Done: false}, nil
		},
		text: "w1 ...",
	}
	tool := stubTool(batch)

	var firstOut string
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		firstOut, firstErr = tool.Execute(context.Background(), `{}`)
	}()
	<-entered

	res, err := executeSupervise(t, tool, `{}`)
	if err != nil {
		t.Fatalf("concurrent Execute: %v, want the busy reply", err)
	}
	if !res.Busy {
		t.Errorf("busy = false, want true for a call that arrived mid-flight")
	}
	if batch.calls != 1 {
		t.Errorf("batch.Next calls = %d, want 1: the refused call must not tick", batch.calls)
	}

	close(release)
	<-done
	if firstErr != nil {
		t.Fatalf("first Execute: %v", firstErr)
	}
	var first superviseResult
	if err := json.Unmarshal([]byte(firstOut), &first); err != nil {
		t.Fatalf("first reply is not JSON: %v\n%s", err, firstOut)
	}
	if first.Busy {
		t.Error("the call that held the flight is reported busy")
	}
}

// TestSuperviseTool_RefusesWithoutCallerOrFactory pins the two configuration
// refusals: no calling scope to bind (the documented failure at
// SuperviseTool.begin) and no batch factory (the supervision engine never
// started). Both are errors, not silent empty replies.
func TestSuperviseTool_RefusesWithoutCallerOrFactory(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool *SuperviseTool
		want string
	}{
		{"no calling scope", &SuperviseTool{}, "no calling scope"},
		{"no batch factory", &SuperviseTool{CallerScopeID: "scope:root"}, "supervision engine is unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.tool.Execute(context.Background(), `{}`)
			if err == nil {
				t.Fatalf("err = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestDescendantSnapshot_FailsClosed pins the filter's conservative failure:
// without a scope tree nothing is provably a descendant, so the batch sees an
// empty membership instead of the whole session's workers.
func TestDescendantSnapshot_FailsClosed(t *testing.T) {
	f := newSuperviseFixture(t)
	worker := f.root("lonely")
	f.runs.Register(worker, steerRun())
	src := &supervision.Snapshotter{Runs: f.runs, Store: f.store}

	for _, tc := range []struct {
		name string
		snap *DescendantSnapshot
	}{
		{"no scope tree", &DescendantSnapshot{Source: src, Caller: "scope:root"}},
		{"no caller", &DescendantSnapshot{Source: src, Scopes: f.mgr}},
	} {
		if got := tc.snap.Snapshot(); len(got.Agents) != 0 {
			t.Errorf("%s: kept %d agent(s), want none", tc.name, len(got.Agents))
		}
	}
}
