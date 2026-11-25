package supervision

// records_test.go pins the intervention record log (yaah-vaxv, design §4.10):
// every decision and every rules action — executed or dropped — is appended with
// its source, tick and outcome; the log orders by tick; the settling run drains
// its own scope's records exactly once; and the trace sink writes them as facts
// on the recorded scope's own owner path.

import (
	"context"
	"fmt"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// ivr builds one record. Only the fields a test asserts on are set here; the log
// never reads the others.
func ivr(tick int, scope, source string, action supervisionapi.Action, outcome, dropReason string) supervisionapi.InterventionRecord {
	return supervisionapi.InterventionRecord{
		At:         testClock().Add(time.Duration(tick) * time.Second),
		Tick:       tick,
		Scope:      scope,
		Source:     source,
		Action:     action,
		Reason:     "reason",
		Guidance:   "guidance",
		Outcome:    outcome,
		DropReason: dropReason,
	}
}

// TestOrderedGroupsTickZeroAtTheFront pins the ordering rule the envelope
// depends on: records are returned in tick order, stably within a tick, and the
// records made BETWEEN ticks (Tick 0 — a direct rules delivery, a queue-full
// drop, a root action) group at the front instead of pretending to belong to a
// tick they were never part of.
func TestOrderedGroupsTickZeroAtTheFront(t *testing.T) {
	in := []supervisionapi.InterventionRecord{
		ivr(2, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		ivr(0, "scope:a", supervisionapi.SourceRoot, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		ivr(1, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		ivr(1, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionRevert, supervisionapi.OutcomeDropped, supervisionapi.DropReasonNotReversible),
		ivr(0, "scope:a", supervisionapi.SourceRules, supervisionapi.ActionStop, supervisionapi.OutcomeDropped, supervisionapi.DropReasonRootScope),
	}

	got := ordered(in)
	wantTicks := []int{0, 0, 1, 1, 2}
	if len(got) != len(wantTicks) {
		t.Fatalf("ordered() returned %d records, want %d", len(got), len(wantTicks))
	}
	for i, want := range wantTicks {
		if got[i].Tick != want {
			t.Fatalf("ordered()[%d].Tick = %d, want %d (full order %v)", i, got[i].Tick, want, ticksOf(got))
		}
	}
	// Stable within a tick: the two tick-1 records keep this file's order, so a
	// reader sees the applied steer before the revert that was dropped.
	if got[2].Action != supervisionapi.ActionSteer || got[3].DropReason != supervisionapi.DropReasonNotReversible {
		t.Errorf("tick 1 records = %+v / %+v, want the input order preserved", got[2], got[3])
	}
	if empty := ordered(nil); empty != nil {
		t.Errorf("ordered(nil) = %v, want nil", empty)
	}
}

// ticksOf is the tick sequence of a record slice, for failure messages.
func ticksOf(recs []supervisionapi.InterventionRecord) []int {
	out := make([]int, len(recs))
	for i, r := range recs {
		out[i] = r.Tick
	}
	return out
}

// TestRegistryRecordsPerScopeAndDrains pins the registry half: records are
// per-scope, a read is a copy, and a drain CLEARS — so the run that settled
// cannot report the same intervention twice.
func TestRegistryRecordsPerScopeAndDrains(t *testing.T) {
	runs := NewRegistry()
	runs.RecordInterventions(
		ivr(2, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		ivr(1, "scope:a", supervisionapi.SourceRules, supervisionapi.ActionSteer, supervisionapi.OutcomeDropped, supervisionapi.DropReasonCooldown),
		ivr(3, "scope:b", supervisionapi.SourcePolicy, supervisionapi.ActionRedirect, supervisionapi.OutcomeClaimed, ""),
	)

	if got := runs.Interventions("nobody"); got != nil {
		t.Errorf("Interventions for an unrecorded scope = %v, want nil", got)
	}
	a := runs.Interventions("scope:a")
	if len(a) != 2 || a[0].Tick != 1 || a[1].Tick != 2 {
		t.Fatalf("scope:a records = %v, want the two records in tick order", ticksOf(a))
	}
	// A read is a copy: rewriting what it returned must not rewrite the log.
	a[0].Reason = "tampered"
	if again := runs.Interventions("scope:a"); again[0].Reason != "reason" {
		t.Errorf("Interventions returned a view into the log (reason = %q)", again[0].Reason)
	}

	drained := runs.DrainInterventions("scope:a")
	if len(drained) != 2 {
		t.Fatalf("drained %d records, want 2", len(drained))
	}
	if left := runs.Interventions("scope:a"); left != nil {
		t.Errorf("records after the drain = %v, want none", left)
	}
	if second := runs.DrainInterventions("scope:a"); second != nil {
		t.Errorf("a second drain = %v, want nil: a later settle of the same scope must not re-report", second)
	}
	if b := runs.Interventions("scope:b"); len(b) != 1 {
		t.Errorf("scope:b records = %v, want its one record untouched by scope:a's drain", ticksOf(b))
	}
}

// TestRecordLogBounds pins both bounds: the per-scope window keeps the NEWEST
// records, and the scope map evicts the oldest scope rather than growing for the
// session's life.
func TestRecordLogBounds(t *testing.T) {
	t.Run("per scope window", func(t *testing.T) {
		runs := NewRegistry()
		recs := make([]supervisionapi.InterventionRecord, 0, maxInterventionRecordsPerScope+8)
		for i := 1; i <= maxInterventionRecordsPerScope+8; i++ {
			recs = append(recs, ivr(i, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
		}
		runs.RecordInterventions(recs...)

		got := runs.Interventions("scope:a")
		if len(got) != maxInterventionRecordsPerScope {
			t.Fatalf("kept %d records, want the %d-record window", len(got), maxInterventionRecordsPerScope)
		}
		if got[0].Tick != 9 || got[len(got)-1].Tick != maxInterventionRecordsPerScope+8 {
			t.Errorf("window ticks %d..%d, want the NEWEST 9..%d kept in order",
				got[0].Tick, got[len(got)-1].Tick, maxInterventionRecordsPerScope+8)
		}
	})

	t.Run("scope map eviction", func(t *testing.T) {
		runs := NewRegistry()
		for i := 0; i <= maxInterventionScopes; i++ {
			runs.RecordInterventions(ivr(1, scopeName(i), supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
		}
		if got := runs.Interventions(scopeName(0)); got != nil {
			t.Errorf("the oldest scope kept %d record(s), want evicted once past %d scopes", len(got), maxInterventionScopes)
		}
		if got := runs.Interventions(scopeName(maxInterventionScopes)); len(got) != 1 {
			t.Errorf("the newest scope kept %d records, want 1", len(got))
		}
	})

	// A drained scope's slot must go back to the pool: drain removes the scope
	// from `order` as well as `byScope`, so when the scope records again (a
	// redirect successor on the same scope, a later rules firing) it re-enters
	// at the BACK of the eviction order. The stale-entry bug this pins (PR #415
	// review finding 3/6): a leftover order key made the reused scope look new
	// while order was already at the cap, so its re-entry popped the oldest
	// LIVE scope early — and the next scope over popped another one, evicting
	// records nobody had drained.
	t.Run("drained scope re-enters without evicting live ones", func(t *testing.T) {
		runs := NewRegistry()
		// Fill order exactly to the cap: scopes 0..max-1, one record each.
		for i := 0; i < maxInterventionScopes; i++ {
			runs.RecordInterventions(ivr(1, scopeName(i), supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
		}
		const reused = 5
		// Drain one scope: its records AND its order slot go away.
		if got := runs.DrainInterventions(scopeName(reused)); len(got) != 1 {
			t.Fatalf("drain returned %d records, want the 1 recorded", len(got))
		}
		// The drained scope records again (successor generation): re-entry at
		// len(order) == cap-1 must NOT evict anything.
		runs.RecordInterventions(ivr(2, scopeName(reused), supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
		if got := runs.Interventions(scopeName(1)); len(got) != 1 {
			t.Errorf("scope 1 holds %d records after the reuse, want its 1 untouched: a drained-then-reused scope must not evict a live one on re-entry", len(got))
		}
		// Now push one scope over the cap: exactly one eviction, and it names
		// the oldest LIVE scope (0), not the reused one and not a stale slot.
		runs.RecordInterventions(ivr(1, scopeName(maxInterventionScopes), supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
		if got := runs.Interventions(scopeName(0)); got != nil {
			t.Errorf("the oldest live scope kept %d record(s), want exactly it evicted at the cap", len(got))
		}
		if got := runs.Interventions(scopeName(1)); len(got) != 1 {
			t.Errorf("scope 1 holds %d records at the cap, want 1: the eviction must name the OLDEST live scope only", len(got))
		}
		if got := runs.Interventions(scopeName(reused)); len(got) != 1 || got[0].Tick != 2 {
			t.Errorf("the reused scope holds %+v, want exactly its post-drain tick-2 record", got)
		}
	})
}

// scopeName names the i-th synthetic scope of the eviction test.
func scopeName(i int) string {
	return "scope:worker-" + itoa(i)
}

// itoa is a tiny decimal formatter, so the test needs no fmt import for its
// failure paths alone.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestNilRegistryRecordsNothing pins the nil-receiver contract every other
// registry method has: a batch built without a registry records nothing rather
// than panicking, and a nil sink leaves the log memory-only.
func TestNilRegistryRecordsNothing(t *testing.T) {
	var runs *Registry
	runs.RecordInterventions(ivr(1, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
	if got := runs.Interventions("scope:a"); got != nil {
		t.Errorf("nil registry returned %v", got)
	}
	if got := runs.DrainInterventions("scope:a"); got != nil {
		t.Errorf("nil registry drained %v", got)
	}
	runs.SetRecordSink(nil)

	memoryOnly := NewRegistry()
	memoryOnly.RecordInterventions(ivr(1, "scope:a", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""))
	if got := memoryOnly.Interventions("scope:a"); len(got) != 1 {
		t.Errorf("memory-only log kept %d records, want 1", len(got))
	}
	if sink := ScopeRecordSink(nil); sink != nil {
		t.Error("ScopeRecordSink(nil) is non-nil, want nil (no scope manager, no owner path)")
	}
}

// TestScopeRecordSinkWritesFactsOnTheOwnerPath is the trace half of yaah-vaxv: a
// record appended to the log lands as one yaah.supervision.intervention.v1 fact
// on the RECORDED scope's own owner path — the same path the shell guard's deny
// fact takes — carrying the record's own fields.
func TestScopeRecordSinkWritesFactsOnTheOwnerPath(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open trace store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	mgr := goatherd.NewScopeManager(store)
	for _, owner := range []string{"sub-worker", "other-worker"} {
		if _, cerr := mgr.Create(owner, nil); cerr != nil {
			t.Fatalf("create scope %s: %v", owner, cerr)
		}
	}

	runs := NewRegistry()
	runs.SetRecordSink(ScopeRecordSink(mgr))
	runs.RecordInterventions(
		ivr(1, "scope:sub-worker", supervisionapi.SourcePolicy, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		ivr(2, "scope:sub-worker", supervisionapi.SourcePolicy, supervisionapi.ActionRevert, supervisionapi.OutcomeDropped, supervisionapi.DropReasonNotReversible),
		ivr(1, "scope:other-worker", supervisionapi.SourceRules, supervisionapi.ActionStop, supervisionapi.OutcomeApplied, ""),
	)

	facts := interventionFacts(t, store, "sub-worker")
	if len(facts) != 2 {
		t.Fatalf("%d facts on the recorded scope's owner path, want 2: %+v", len(facts), facts)
	}
	if got := facts[0].Body.Payload["action"]; got != string(supervisionapi.ActionSteer) {
		t.Errorf("first fact action = %v, want the applied steer", got)
	}
	if got := facts[1].Body.Payload["drop_reason"]; got != supervisionapi.DropReasonNotReversible {
		t.Errorf("second fact drop_reason = %v, want %q", got, supervisionapi.DropReasonNotReversible)
	}
	if got := facts[1].Body.Payload["outcome"]; got != supervisionapi.OutcomeDropped {
		t.Errorf("second fact outcome = %v, want %q", got, supervisionapi.OutcomeDropped)
	}
	if got := facts[0].Body.Payload["source"]; got != supervisionapi.SourcePolicy {
		t.Errorf("first fact source = %v, want %q", got, supervisionapi.SourcePolicy)
	}
	// Payload values round-trip through JSON, so the tick arrives as a
	// json.Number rather than an int.
	if got := facts[0].Body.Payload["tick"]; fmt.Sprint(got) != "1" {
		t.Errorf("first fact tick = %v (%T), want 1", got, got)
	}
	if view := facts[0].View; view != nil && view.KindLabel != SupervisionInterventionKind {
		t.Errorf("fact kind = %q, want %q", view.KindLabel, SupervisionInterventionKind)
	}

	// A fact lands on the scope the record NAMED, not on whichever scope
	// happened to be recorded first.
	if other := interventionFacts(t, store, "other-worker"); len(other) != 1 {
		t.Errorf("%d facts on the other scope, want its own one", len(other))
	}
}

// interventionFacts reads the intervention facts recorded on one owner path.
func interventionFacts(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner string) []kerneltrace.Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, 100, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", owner, err)
	}
	var out []kerneltrace.Record
	for _, id := range slice.FactIDs() {
		if rec, ok := slice.FactsByID[id].(kerneltrace.Record); ok && rec.Envelope.SchemaRef == SchemaSupervisionIntervention {
			out = append(out, rec)
		}
	}
	return out
}

// TestBatchRecordsEveryDecisionWithSource pins the batch half: a tick's executed
// AND dropped decisions both reach the log with Source=policy, their tick
// numbers set, and the outcome vocabulary the arbiter already uses.
func TestBatchRecordsEveryDecisionWithSource(t *testing.T) {
	// CooldownTicks: -1 selects the documented default (one tick); leaving it
	// zero would DISABLE the cooldown and the tick-2 drop below would apply.
	h := newBatchHarness(t, BatchConfig{
		Actions:       []supervisionapi.Action{supervisionapi.ActionSteer},
		CooldownTicks: -1,
	})
	h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
	h.run("scope:a")
	h.run("scope:b")

	// Tick 1: one applied steer, one redirect the action mask refuses.
	h.next(
		supervisionapi.Decision{Scope: "scope:a", Action: supervisionapi.ActionSteer, Guidance: "tighten the diff"},
		supervisionapi.Decision{Scope: "scope:b", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
	)
	// Tick 2: the cooldown refuses a second steer on scope:a.
	h.next(supervisionapi.Decision{Scope: "scope:a", Action: supervisionapi.ActionSteer, Guidance: "again"})

	a := h.runs.Interventions("scope:a")
	if len(a) != 2 {
		t.Fatalf("scope:a records = %+v, want the applied steer and the cooldown drop", a)
	}
	if a[0].Tick != 1 || a[0].Outcome != supervisionapi.OutcomeApplied || a[0].Action != supervisionapi.ActionSteer {
		t.Errorf("first record = %+v, want the applied steer at tick 1", a[0])
	}
	if a[0].Guidance != "tighten the diff" {
		t.Errorf("guidance = %q, want the decision's guidance", a[0].Guidance)
	}
	if a[1].Tick != 2 || a[1].Outcome != supervisionapi.OutcomeDropped || a[1].DropReason != supervisionapi.DropReasonCooldown {
		t.Errorf("second record = %+v, want the cooldown drop at tick 2", a[1])
	}

	b := h.runs.Interventions("scope:b")
	if len(b) != 1 || b[0].DropReason != supervisionapi.DropReasonMaskedAction || b[0].Tick != 1 {
		t.Fatalf("scope:b records = %+v, want the masked-action drop at tick 1", b)
	}

	for _, rec := range append(append([]supervisionapi.InterventionRecord(nil), a...), b...) {
		if rec.Source != supervisionapi.SourcePolicy {
			t.Errorf("record %+v has source %q, want %q", rec, rec.Source, supervisionapi.SourcePolicy)
		}
		if rec.At.IsZero() {
			t.Errorf("record %+v has no timestamp", rec)
		}
	}
}

// TestBatchRecordsQueueFullDrop pins the drop that used to be invisible: a rule
// firing the full queue discards is recorded with the queue_full reason, Tick 0
// (the refusal happens between ticks, on the rules engine's goroutine) and
// Source=rules.
func TestBatchRecordsQueueFullDrop(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true))
	h.run("scope:a")

	for i := 0; i <= maxQueuedRules; i++ {
		h.batch.QueueRuleSteer("scope:a", ruleSteerReason, "guidance")
	}
	if got := h.batch.RulesDropped(); got != 1 {
		t.Fatalf("RulesDropped = %d, want the one firing the full queue discarded", got)
	}

	recs := h.runs.Interventions("scope:a")
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want exactly the discarded firing", recs)
	}
	rec := recs[0]
	if rec.Tick != 0 || rec.Source != supervisionapi.SourceRules || rec.Action != supervisionapi.ActionSteer {
		t.Errorf("record = %+v, want a tick-0 rules steer", rec)
	}
	if rec.Outcome != supervisionapi.OutcomeDropped || rec.DropReason != supervisionapi.DropReasonQueueFull {
		t.Errorf("outcome = %q/%q, want dropped/%q", rec.Outcome, rec.DropReason, supervisionapi.DropReasonQueueFull)
	}
}

// newRecordService builds a service over a real trace store with a root scope
// and one forked worker scope, returning the registry so a test can read what the
// DIRECT delivery path recorded. No arbiter is set: this is the no-batch wiring.
func newRecordService(t *testing.T, haltRoot bool) (*Service, *kerneltrace.SQLiteTraceStore, *Registry, chan string) {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open trace store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	bus := kerneltrace.NewEffectBus(64)
	t.Cleanup(bus.Close)
	store.WithBus(bus)

	mgr := goatherd.NewScopeManager(store)
	for _, owner := range []string{"sess-root", "sub-worker"} {
		if _, cerr := mgr.Create(owner, nil); cerr != nil {
			t.Fatalf("create scope %s: %v", owner, cerr)
		}
	}
	runs := NewRegistry()
	steer := make(chan string, SteerBuffer)
	runs.Register("scope:sess-root", Run{SteerCh: make(chan string, SteerBuffer), Cancel: func() {}})
	runs.Register("scope:sub-worker", Run{SteerCh: steer, Cancel: func() {}})

	return NewService(mgr, bus, runs, AdvisoryRules{}, false, haltRoot, "scope:sess-root"), store, runs, steer
}

// TestServiceRecordsDirectDeliveryRulesActions pins the rules half on the path
// that has NO batch bound (design §4.10's constraint 6): every action the rules
// engine delivers itself is recorded with Source=rules — applied and dropped
// alike — with Tick 0, because no tick arbitrated it.
func TestServiceRecordsDirectDeliveryRulesActions(t *testing.T) {
	t.Run("inject applied", func(t *testing.T) {
		svc, _, runs, steer := newRecordService(t, false)
		gen, _ := runs.Generation("scope:sub-worker")

		svc.deliverInject("scope:sub-worker", "try the narrower fix")

		select {
		case got := <-steer:
			if got != "try the narrower fix" {
				t.Fatalf("delivered %q", got)
			}
		default:
			t.Fatal("the inject never reached the run's steering channel")
		}
		recs := runs.Interventions("scope:sub-worker")
		if len(recs) != 1 {
			t.Fatalf("records = %+v, want one", recs)
		}
		rec := recs[0]
		if rec.Source != supervisionapi.SourceRules || rec.Action != supervisionapi.ActionSteer || rec.Outcome != supervisionapi.OutcomeApplied {
			t.Errorf("record = %+v, want an applied rules steer", rec)
		}
		if rec.Tick != 0 {
			t.Errorf("tick = %d, want 0 (no batch arbitrated this delivery)", rec.Tick)
		}
		if rec.Generation != gen {
			t.Errorf("generation = %d, want the live registration's %d", rec.Generation, gen)
		}
	})

	t.Run("inject with no run", func(t *testing.T) {
		svc, _, runs, _ := newRecordService(t, false)
		runs.Unregister("scope:sub-worker")

		svc.deliverInject("scope:sub-worker", "steer into the void")

		recs := runs.Interventions("scope:sub-worker")
		if len(recs) != 1 || recs[0].Outcome != supervisionapi.OutcomeDropped || recs[0].DropReason != supervisionapi.DropReasonNoRun {
			t.Fatalf("records = %+v, want the no_run drop", recs)
		}
	})

	t.Run("inject into a full buffer", func(t *testing.T) {
		svc, _, runs, steer := newRecordService(t, false)
		for i := 0; i < SteerBuffer; i++ {
			steer <- "queued"
		}

		svc.deliverInject("scope:sub-worker", "one too many")

		recs := runs.Interventions("scope:sub-worker")
		if len(recs) != 1 || recs[0].DropReason != supervisionapi.DropReasonSteerBusy {
			t.Fatalf("records = %+v, want the steer_busy drop", recs)
		}
	})

	t.Run("halt delivered", func(t *testing.T) {
		svc, _, runs, _ := newRecordService(t, false)

		svc.deliverHalt("scope:sub-worker", "stuck in a loop")

		recs := runs.Interventions("scope:sub-worker")
		if len(recs) != 1 {
			t.Fatalf("records = %+v, want one", recs)
		}
		if recs[0].Action != supervisionapi.ActionStop || recs[0].Outcome != supervisionapi.OutcomeApplied {
			t.Errorf("record = %+v, want an applied stop", recs[0])
		}
		if recs[0].Reason != "stuck in a loop" {
			t.Errorf("reason = %q, want the firing's own reason", recs[0].Reason)
		}
	})

	t.Run("halt refused at the root gate", func(t *testing.T) {
		svc, _, runs, _ := newRecordService(t, false)

		svc.deliverHalt("scope:sess-root", "stop the session")

		recs := runs.Interventions("scope:sess-root")
		if len(recs) != 1 {
			t.Fatalf("records = %+v, want the refusal recorded", recs)
		}
		if recs[0].Outcome != supervisionapi.OutcomeDropped || recs[0].DropReason != supervisionapi.DropReasonStopRefused {
			t.Errorf("record = %+v, want dropped/stop_refused", recs[0])
		}
	})
}
