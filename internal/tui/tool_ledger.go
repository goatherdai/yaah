package tui

import (
	"sync"

	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// toolLoss is what is known about one tool call in one turn.
//
// The four booleans/values are the whole vocabulary: an end that arrived before
// its block existed (hasEnd), an end whose UI update was dropped (endLost), and
// a start whose UI update was dropped (startDropped). Every combination is a
// state the view can act on; none of them is inferred from another structure
// (yaah-ol3r).
type toolLoss struct {
	gen          uint64
	end          transcript.ToolEnd
	hasEnd       bool
	endLost      bool
	startDropped bool
}

// toolLedger is the single record of what became of the current turn's tool
// calls, plus any entry from a just-closed turn that a start may still claim.
//
// It replaces three coupled structures that used to do this job between them —
// the transcript's unmatched-end stash, the App's lost-end slice, and the
// re-add rule at the sweep — whose interactions produced a review finding in five
// consecutive rounds (yaah-ol3r).
//
// The generation is what makes it sound. App.turnGen is bumped when the
// DoneEvent is *handled* (on the broker's forwarder goroutine, in event order),
// not when the sweep closure later runs on the UI goroutine. So a turn's records
// are stamped before the next turn's events can be handled, and every lookup
// matches on (gen, key) rather than key alone. A record can therefore never be
// consumed by another turn, and a keyless call — whose fallback key is a
// run-scoped ordinal that repeats every prompt — is as safe as a keyed one.
//
// Access is mutex-guarded because recording happens on the forwarder goroutine
// (where an update's scheduling verdict is known) while consumption happens on
// the UI goroutine (where the transcript is owned).
type toolLedger struct {
	mu      sync.Mutex
	entries map[string]*toolLoss
	// swept records which turns actually ran their sweep. A record from a turn
	// whose sweep ran cannot explain a block sealed later — that turn's own
	// sweep would have sealed it — so the stale record must not suppress the
	// later attribution. A record from a turn whose sweep was LOST is the
	// opposite case: the seal we are attributing now is that turn's block
	// arriving late, and claiming it as swept would count one loss twice
	// (yaah-ol3r review).
	swept map[uint64]bool
}

// ledgerBackstop bounds the map when sweeps stop arriving altogether. It is a
// safety net, not a policy: normal operation retires entries at their own turn's
// sweep, so nothing accumulates across turns even when a sweep is dropped (the
// next one retires the stale generation before it attributes anything).
const ledgerBackstop = 512

func newToolLedger() *toolLedger {
	return &toolLedger{
		entries: make(map[string]*toolLoss),
		swept:   make(map[uint64]bool),
	}
}

// entry returns the record for (gen, key), creating it when absent. A record
// from another generation under the same key is replaced, never reused: that
// replacement is exactly what keeps a repeated ordinal from leaking across turns.
func (l *toolLedger) entry(gen uint64, key string) *toolLoss {
	if e := l.entries[key]; e != nil && e.gen == gen {
		return e
	}
	if l.entries[key] == nil && len(l.entries) >= ledgerBackstop {
		l.evictOldest()
	}
	e := &toolLoss{gen: gen}
	l.entries[key] = e
	return e
}

// evictOldest drops one entry from the oldest generation present. Called only
// when the backstop is exceeded, which means sweeps have not been running.
func (l *toolLedger) evictOldest() {
	oldestKey, oldestGen, found := "", uint64(0), false
	for key, e := range l.entries {
		if !found || e.gen < oldestGen {
			oldestKey, oldestGen, found = key, e.gen, true
		}
	}
	if found {
		delete(l.entries, oldestKey)
	}
}

// stashEnd records an end that settled no block: its start is late (or was lost),
// and a block created later must inherit this end rather than wait for an event
// that is already behind it.
func (l *toolLedger) stashEnd(gen uint64, key string, end transcript.ToolEnd) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(gen, key)
	e.end, e.hasEnd = end, true
}

// markEndLost records that key's end update could never be scheduled, so no end
// will ever be applied for it.
func (l *toolLedger) markEndLost(gen uint64, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(gen, key).endLost = true
}

// markStartDropped records that key's start update could never be scheduled, so
// no block will ever be created for it — and nothing waits for its end.
func (l *toolLedger) markStartDropped(gen uint64, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(gen, key).startDropped = true
}

// takeForStart returns what a start for (gen, key) must inherit and retires the
// entry, since the start is the last party that could act on it. It reports
// false when the ledger knows nothing about this call.
func (l *toolLedger) takeForStart(gen uint64, key string) (toolLoss, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil || e.gen != gen {
		return toolLoss{}, false
	}
	if !e.hasEnd && !e.endLost {
		return toolLoss{}, false
	}
	out := *e
	delete(l.entries, key)
	return out, true
}

// closeTurn retires the entries the sweep for gen is responsible for, records
// that gen was swept, and returns a snapshot of every entry at or below gen so
// the caller can attribute what it seals without holding the lock.
//
// Retirement is deliberately conservative, because two paths can deliver work out
// of order: the managed uiEventCh is FIFO, but the critical fallback runs its
// closures through tview's QueueUpdate from their own goroutines
// (internal/tui/event_queue.go), so a later turn's sweep can be delivered ahead
// of an earlier turn's start that is still waiting in the managed queue. An entry
// is therefore retired only when nothing can need it:
//
//   - its start was dropped, so no block will ever exist to inherit anything;
//   - or it holds neither an end nor news of a lost one, so there is nothing to
//     hand over and nothing to explain a seal with.
//
// Everything else is kept, whatever its generation. Its start may still arrive
// (matched by generation in takeForStart, so a later turn's start cannot claim
// it), and its loss may still need to explain a block that a later sweep has to
// seal — see sweepTurn's attribution. The map is bounded by ledgerBackstop.
func (l *toolLedger) closeTurn(gen uint64) map[string]toolLoss {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.swept[gen] = true

	out := make(map[string]toolLoss, len(l.entries))
	for key, e := range l.entries {
		if e.gen > gen {
			continue // a later turn's record: not this sweep's business
		}
		out[key] = *e
		if e.startDropped || (!e.hasEnd && !e.endLost) {
			delete(l.entries, key)
		}
	}
	l.pruneSwept()
	return out
}

// didSweep reports whether gen's turn-end reconciliation actually ran.
func (l *toolLedger) didSweep(gen uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.swept[gen]
}

// spend retires the record for (gen, key) once it has explained a seal.
func (l *toolLedger) spend(gen uint64, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[key]; e != nil && e.gen == gen {
		delete(l.entries, key)
	}
}

// pruneSwept drops sweep records older than the oldest live entry, so the set
// stays bounded by the entries it can still describe.
func (l *toolLedger) pruneSwept() {
	if len(l.swept) == 0 {
		return
	}
	oldest, found := uint64(0), false
	for _, e := range l.entries {
		if !found || e.gen < oldest {
			oldest, found = e.gen, true
		}
	}
	for gen := range l.swept {
		if !found || gen < oldest {
			delete(l.swept, gen)
		}
	}
}

// forget drops everything — a cleared transcript names blocks that no longer
// exist, so no entry may settle one.
func (l *toolLedger) forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make(map[string]*toolLoss)
	l.swept = make(map[uint64]bool)
}

// pending reports how many entries the ledger holds. Diagnostics only.
func (l *toolLedger) pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
