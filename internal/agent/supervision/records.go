// records.go is the session's intervention record log (yaah-vaxv, design §4.10
// "Record"): every decision the arbiter acts on and every rules action — taken
// or dropped — is appended here as it is made, and the run a record names drains
// its scope's records when it settles, so the dispatch envelope can carry them
// beside its typed stop outcome (jobs.SubAgentStop).
//
// Why the log hangs off the Registry: its two producers and its one reader
// already share exactly one object. The Batch (policy decisions plus the rules
// actions it arbitrates) holds the registry, the Service (a rules action
// delivered directly, with no batch bound at all) holds the registry, and the
// sub-agent runner drains from it at settle time. Everything else in that
// three-way is a new wiring path.
//
// A record also has to OUTLIVE the registration it names: a decision dropped
// not_live is about a run that already finished, which is precisely the record a
// reader most wants. So the log is its own bounded map, not a field on the run
// entry, and unregistering a run does not throw its records away.
//
// The trace half is a SINK rather than the log's own store (SetRecordSink): the
// composition root installs a writer that appends each record as a fact on the
// recorded scope's own owner path, so the operator's `trace` view and the
// model's envelope never disagree about what happened. Nil is the memory-only
// wiring, which is what a batch built without a trace store has.
package supervision

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

const (
	// maxInterventionRecordsPerScope bounds one scope's records: the newest are
	// kept and the oldest are dropped, because a settle reply is read to learn
	// what just happened (the supervise tool's retainTick makes the same call).
	// The bound exists because a scope can be supervised for the whole session
	// and a lost drain must not accumulate without limit.
	maxInterventionRecordsPerScope = 32
	// maxInterventionScopes bounds how many scopes the log keeps records for.
	// Past it the oldest scope's records are evicted whole. The realistic
	// session has single digits; the bound is for a long-lived session whose
	// dispatches churn.
	maxInterventionScopes = 256
)

// interventionLog is the per-scope record log behind Registry. Its own mutex,
// never Registry.mu: a producer can hold no other lock while it appends, and the
// sink a record triggers may do store I/O.
type interventionLog struct {
	mu      sync.Mutex
	byScope map[string][]supervisionapi.InterventionRecord
	// order is the scope keys in first-write order, so eviction past
	// maxInterventionScopes is "the oldest scope's records go first" without a
	// timestamp per scope.
	order []string
	// sink, when set, receives every appended record after the append lands —
	// the trace half. It is called OUTSIDE the lock: a sink writes facts.
	sink func([]supervisionapi.InterventionRecord)
}

// append adds records to their scopes' logs, keeping the per-scope window.
//
// Records arrive already in production order, which for a tick is tick order:
// the arbiter builds a tick's records in one pass and hands them over together.
//
// A record VALUE reaches the log at most once. The stop path hands the same
// record twice on purpose: the delivery side appends it at the CANCEL, which is
// the only point ahead of the halted run's settle (see BatchConfig.Stop), and
// the tick result then carries the same value to the caller. Counted twice, one
// stop would read as two, and the envelope is a report of what happened. Two
// genuinely distinct records are never equal here — every record carries its own
// At timestamp — so this is idempotence, not deduplication of real events.
func (l *interventionLog) append(recs []supervisionapi.InterventionRecord) {
	if len(recs) == 0 {
		return
	}
	added := make([]supervisionapi.InterventionRecord, 0, len(recs))
	l.mu.Lock()
	if l.byScope == nil {
		l.byScope = make(map[string][]supervisionapi.InterventionRecord, 8)
	}
	for _, rec := range recs {
		existing, ok := l.byScope[rec.Scope]
		if !ok {
			l.evictOldestLocked()
			l.order = append(l.order, rec.Scope)
		}
		if holdsRecord(existing, rec) {
			continue
		}
		existing = append(existing, rec)
		if over := len(existing) - maxInterventionRecordsPerScope; over > 0 {
			// Copy the survivors down in place: the window keeps the NEWEST
			// records, still in production order.
			existing = append(existing[:0], existing[over:]...)
		}
		l.byScope[rec.Scope] = existing
		added = append(added, rec)
	}
	sink := l.sink
	l.mu.Unlock()

	// Outside the lock: the sink writes facts, and a stalled store must never
	// hold the log against a concurrent producer.
	if sink != nil {
		sink(added)
	}
}

// holdsRecord reports whether the scope's window already holds this exact
// record value. A record is a value of comparable fields, so equality is the
// whole record: same instant, same tick, same action, same outcome.
func holdsRecord(window []supervisionapi.InterventionRecord, rec supervisionapi.InterventionRecord) bool {
	for _, have := range window {
		if have == rec {
			return true
		}
	}
	return false
}

// evictOldestLocked drops the records of the scope that was written first.
// Callers hold l.mu.
func (l *interventionLog) evictOldestLocked() {
	if len(l.order) < maxInterventionScopes {
		return
	}
	oldest := l.order[0]
	l.order = append(l.order[:0], l.order[1:]...)
	delete(l.byScope, oldest)
}

// forScope returns a scope's records in tick order, oldest tick first. It is a
// copy: a caller cannot rewrite the log through it.
func (l *interventionLog) forScope(scopeID string) []supervisionapi.InterventionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return ordered(l.byScope[scopeID])
}

// drain returns a scope's records in tick order AND clears them, so the run that
// just settled cannot report the same intervention twice (a redirect successor
// is a run of its own, and it opens its own account).
//
// The scope's eviction-order entry goes with it. Leaving it behind is a live
// bug, not untidiness: a scope that records again after a drain looks NEW to
// append but is still OLD to eviction, so it never gets a fresh position — and
// eviction can then pop its stale entry and delete a live scope's records while
// the records of a scope past the cap survive. The invariant that makes
// eviction honest is that order names exactly the scopes byScope holds.
func (l *interventionLog) drain(scopeID string) []supervisionapi.InterventionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs := ordered(l.byScope[scopeID])
	if _, ok := l.byScope[scopeID]; !ok {
		return recs
	}
	delete(l.byScope, scopeID)
	l.dropOrderLocked(scopeID)
	return recs
}

// dropOrderLocked removes one scope from the eviction order, keeping the order
// of the rest. Callers hold l.mu.
func (l *interventionLog) dropOrderLocked(scopeID string) {
	for i, id := range l.order {
		if id == scopeID {
			l.order = append(l.order[:i], l.order[i+1:]...)
			return
		}
	}
}

// setSink installs the trace writer (nil removes it).
func (l *interventionLog) setSink(sink func([]supervisionapi.InterventionRecord)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sink = sink
}

// ordered copies recs into tick order, stably: records of one tick keep the
// order the tick built them in, and the records made BETWEEN ticks (Tick 0, the
// direct-delivery and queue-full drops) group at the front where the envelope
// renders them as "outside the tick loop".
//
// The sort is needed because the producers are concurrent: a Service firing
// delivered directly and a batch tick both append, and their interleaving on the
// log says nothing about which tick they belong to.
func ordered(recs []supervisionapi.InterventionRecord) []supervisionapi.InterventionRecord {
	if len(recs) == 0 {
		return nil
	}
	out := append([]supervisionapi.InterventionRecord(nil), recs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tick < out[j].Tick })
	return out
}

// RecordInterventions appends intervention records to the session's log: the
// batch and the rules engine call it as they act, and the runner drains what its
// scope accumulated when the run settles (yaah-vaxv).
//
// Safe on a nil receiver and on a zero Registry: a batch built without a
// registry records nothing rather than panicking (the same "nil means no runs
// bridged" contract every other registry method has).
func (r *Registry) RecordInterventions(recs ...supervisionapi.InterventionRecord) {
	if r == nil {
		return
	}
	r.records.append(recs)
}

// Interventions returns the records accumulated for one scope, in tick order,
// without clearing them. It is the read-only half a status surface wants; the
// runner uses DrainInterventions.
func (r *Registry) Interventions(scopeID string) []supervisionapi.InterventionRecord {
	if r == nil || scopeID == "" {
		return nil
	}
	return r.records.forScope(scopeID)
}

// DrainInterventions returns one scope's records in tick order and clears them.
// The runner calls it at settle, once per run: the records it returns are the
// ones the envelope cites, and clearing them is what keeps a later run of the
// same scope from inheriting them.
func (r *Registry) DrainInterventions(scopeID string) []supervisionapi.InterventionRecord {
	if r == nil || scopeID == "" {
		return nil
	}
	return r.records.drain(scopeID)
}

// SetRecordSink installs the trace writer for the session's intervention
// records (nil restores memory-only). It receives every appended record, after
// the append lands; ScopeRecordSink is the production writer.
//
// Safe to call at any time. The sink is called outside the log's lock, but
// SYNCHRONOUSLY on the producer's goroutine, which is why ScopeRecordSink bounds
// its own write.
func (r *Registry) SetRecordSink(sink func([]supervisionapi.InterventionRecord)) {
	if r == nil {
		return
	}
	r.records.setSink(sink)
}

// SchemaSupervisionIntervention is the trace schema ref for one supervision
// intervention record: the durable half of the record the dispatch envelope
// carries (yaah-vaxv). The payload is the record's own field set, so a reader of
// the trace and a reader of the envelope are reading one record.
const SchemaSupervisionIntervention = "yaah.supervision.intervention.v1"

// SupervisionInterventionKind is the kind label the intervention fact carries.
const SupervisionInterventionKind = "supervision:intervention"

const (
	// recordFactTimeout bounds ONE record's fact write. Per record, not per
	// batch: one stalled write must not swallow the records behind it, and a
	// batch of a dozen records written under a single two-second bound was
	// spending the overflow of one slow write on the next one (PR #415 review
	// finding 7).
	recordFactTimeout = 500 * time.Millisecond
	// recordFactBatchTimeout bounds one sink call as a whole, so the per-record
	// bound cannot multiply into an unbounded stall on a producer's goroutine:
	// a batch whose store never answers costs at most this much, whatever its
	// length.
	recordFactBatchTimeout = 2 * time.Second
)

// recordFactFailure makes a broken trace store visible ONCE per process instead
// of never. Every record is already carried by the log and the dispatch
// envelope, so a lost fact is a diagnostic regression and not a lost
// intervention — which is exactly why it must not be silent, and exactly why it
// must not warn on every tick of a long session (the same first-failure shape
// the arbiter's noStopOnce uses).
var recordFactFailure sync.Once

// noteRecordFactFailure warns about the first fact that did not reach the trace
// and debug-logs the rest. scope is the recorded scope (empty when the record
// was made for a scope the log knows nothing about).
func noteRecordFactFailure(reason string, rec supervisionapi.InterventionRecord, err error) {
	recordFactFailure.Do(func() {
		slog.Warn("supervision: an intervention record did not reach the trace; the session log and the dispatch envelope still carry it",
			"reason", reason, "scope", rec.Scope, "tick", rec.Tick, "err", err)
	})
	slog.Debug("supervision: intervention record not in the trace",
		"reason", reason, "scope", rec.Scope, "tick", rec.Tick, "err", err)
}

// ScopeRecordSink returns the writer Registry.SetRecordSink takes: each record
// becomes one SchemaSupervisionIntervention fact on the NAMED scope's own trace
// path, where the scope's other facts live (the same path the shell guard's deny
// fact takes).
//
// Synchronous on purpose, and the trade-off is deliberate. This package's A2
// invariant is that the fact is the record, and an intervention that only exists
// in memory until some run settles is that inversion in slower motion — a scope
// whose run dies without settling (a timeout that never returns, an operator
// stopping the session) would leave no trace of what the supervisor did to it.
// The sink is called inline from the producer, so a stalled store can delay a
// tick: the bound is per record (recordFactTimeout) inside a whole-batch bound
// (recordFactBatchTimeout), which caps that cost at a couple of seconds for a
// tick whose cadence is seconds anyway, and durability is worth more than
// latency here. A background queue would buy the difference and cost a lifecycle
// — a bounded queue, an owner, and a join at session teardown — that this seat
// does not have; the supervision Service's deny-fact writers carry that
// machinery only because they are created at wiring time and joined by Close.
//
// nil mgr yields nil: without a scope manager no scope is addressable, so there
// is no owner path to record on and the log stays memory-only.
func ScopeRecordSink(mgr *goatherd.ScopeManager) func([]supervisionapi.InterventionRecord) {
	if mgr == nil {
		return nil
	}
	return func(recs []supervisionapi.InterventionRecord) {
		if len(recs) == 0 {
			return
		}
		batchCtx, cancelBatch := context.WithTimeout(context.Background(), recordFactBatchTimeout)
		defer cancelBatch()
		for _, rec := range recs {
			scope, ok := mgr.Get(rec.Scope)
			if !ok {
				noteRecordFactFailure("scope not addressable", rec, nil)
				continue
			}
			ctx, cancel := context.WithTimeout(batchCtx, recordFactTimeout)
			_, err := scope.AppendFact(ctx, SchemaSupervisionIntervention, SupervisionInterventionKind, interventionPayload(rec))
			cancel()
			if err != nil {
				noteRecordFactFailure("append failed", rec, err)
			}
		}
	}
}

// interventionPayload is the fact body: the record's own fields, under the names
// the record declares, so the two surfaces cite one shape.
func interventionPayload(rec supervisionapi.InterventionRecord) map[string]any {
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	return map[string]any{
		"at":          at.UTC().Format(time.RFC3339Nano),
		"tick":        rec.Tick,
		"scope":       rec.Scope,
		"source":      rec.Source,
		"action":      string(rec.Action),
		"reason":      rec.Reason,
		"guidance":    rec.Guidance,
		"outcome":     rec.Outcome,
		"drop_reason": rec.DropReason,
		"generation":  rec.Generation,
	}
}
