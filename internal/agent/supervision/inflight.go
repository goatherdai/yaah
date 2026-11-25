// inflight.go tracks one scope's started-but-not-returned tool calls
// (yaah-k0x1, design §4.2): the host's own tool-start and tool-end events
// feed a per-registration table the registry holds, and the snapshot
// builder copies its entries into AgentSnapshot.InFlight. The table is
// memory only — nothing here is persisted, so a host restart loses nothing
// that still exists — and a table whose registration is gone is inert:
// a late forwarder's writes land in a map nobody reads.
package supervision

import (
	"sort"
	"sync"
	"time"

	"github.com/goatherdai/yaah/internal/agent/events"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// inflightEntry is one open call's fixed facts.
type inflightEntry struct {
	name      string
	args      string
	startedAt time.Time
}

// InFlightTable is one scope's table of tool calls that have started and
// not returned. The registry creates one per registration and drops it
// with the run; the dispatch runner feeds it through Observe, and the
// snapshot builder reads it through the registry's InFlightCalls.
type InFlightTable struct {
	mu   sync.Mutex
	open map[int64]inflightEntry
	// now stamps a Start. A field, not a direct time.Now call, so a test
	// can pin the clock.
	now func() time.Time
}

// newInFlightTable returns an empty table. The entry map is allocated on
// the first Start, so an idle table — most scopes, most of the time —
// costs one small struct.
func newInFlightTable(now func() time.Time) *InFlightTable {
	if now == nil {
		now = time.Now
	}
	return &InFlightTable{now: now}
}

// Start records a call whose execution has begun. The args are the start
// event's abbreviated text, clipped here with the snapshot's existing args
// cap: the table stores bounded text or nothing.
func (t *InFlightTable) Start(id int64, name, args string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = make(map[int64]inflightEntry)
	}
	t.open[id] = inflightEntry{name: name, args: clipLine(args, maxSnapshotArgsBytes), startedAt: at}
}

// End removes the call whose execution returned. Removing an ID that never
// started, or already ended, is a no-op: an end without its start carries
// no information a snapshot could have shown anyway.
func (t *InFlightTable) End(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, id)
}

// Clear drops every open call. The registry calls it when a run's loop
// has returned (MarkReturned): nothing is executing in the scope
// anymore, and a call whose end event was lost to the loop's closing
// broker must not linger and age as if hung across the adoption window.
func (t *InFlightTable) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open = nil
}

// Entries snapshots the open calls, oldest first, each with its age at
// now. Nil when nothing is open, so a snapshot reports no in-flight calls
// rather than an empty slice; a negative age — a caller's clock behind a
// start's stamp — clamps to zero rather than going backwards.
func (t *InFlightTable) Entries(now time.Time) []supervisionapi.InFlightCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.open) == 0 {
		return nil
	}
	entries := make([]inflightEntry, 0, len(t.open))
	for _, e := range t.open {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].startedAt.Before(entries[j].startedAt) })
	out := make([]supervisionapi.InFlightCall, 0, len(entries))
	for _, e := range entries {
		age := now.Sub(e.startedAt)
		if age < 0 {
			age = 0
		}
		out = append(out, supervisionapi.InFlightCall{Name: e.name, Args: e.args, Age: age})
	}
	return out
}

// Observe feeds one loop event into the table: a ToolStartEvent opens a
// call, the matching ToolEndEvent (same per-execution ID) closes it, and
// everything else is ignored. The broker delivers HandleEvent sequentially
// on its forwarder goroutine, so Observe never runs concurrently with
// itself; every method still takes the mutex, because Entries is read from
// the snapshot's caller. Bounded by construction: args are clipped at
// Start, and a start without an end — a call cut off by cancellation, which
// no run publishes an end event for — stays one small entry until the
// registration drops the whole table.
func (t *InFlightTable) Observe(ev events.Event) {
	switch e := ev.(type) {
	case *events.ToolStartEvent:
		t.Start(e.ID, e.Name, e.Args, t.now())
	case *events.ToolEndEvent:
		t.End(e.ID)
	}
}
