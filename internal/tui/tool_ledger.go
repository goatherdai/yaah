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
}

// ledgerBackstop bounds the map when sweeps stop arriving altogether. It is a
// safety net, not a policy: normal operation retires entries at their own turn's
// sweep, so nothing accumulates across turns even when a sweep is dropped (the
// next one retires the stale generation before it attributes anything).
const ledgerBackstop = 512

func newToolLedger() *toolLedger {
	return &toolLedger{entries: make(map[string]*toolLoss)}
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

// closeTurn retires the entries the sweep for gen is responsible for, and returns
// a snapshot of every entry at or below gen so the caller can attribute what it
// seals without holding the lock.
//
// Retiring rules, one per state:
//
//   - gen  < gen: a previous turn's record. No start can still arrive for it —
//     the UI queue is FIFO, so a start enqueued before this sweep has already
//     run — and its loss, if any, was attributed by that turn's own sweep or
//     never will be. Dropped here instead of suppressing this turn's attribution.
//   - gen == gen, start not dropped, and something to hand to a start (an end,
//     or news that the end was lost): KEPT. The sweep can run while a start from
//     this same turn is still in flight (the critical fallback runs the
//     reconciliation directly, ahead of a start still in the queue), and that
//     start must create its block already settled — or already interrupted when
//     the end was lost, which is the only thing that keeps it off a permanent
//     chevron.
//   - everything else at gen: spent. The start was itself dropped, so no block
//     will exist to inherit anything, or nothing is known to hand over.
func (l *toolLedger) closeTurn(gen uint64) map[string]toolLoss {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]toolLoss, len(l.entries))
	for key, e := range l.entries {
		if e.gen > gen {
			continue // a later turn's record: not this sweep's business
		}
		out[key] = *e
		switch {
		case e.gen < gen:
			delete(l.entries, key)
		case !e.startDropped && (e.hasEnd || e.endLost):
			// keep for a start that may still land
		default:
			delete(l.entries, key)
		}
	}
	return out
}

// forget drops everything — a cleared transcript names blocks that no longer
// exist, so no entry may settle one.
func (l *toolLedger) forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make(map[string]*toolLoss)
}

// pending reports how many entries the ledger holds. Diagnostics only.
func (l *toolLedger) pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
