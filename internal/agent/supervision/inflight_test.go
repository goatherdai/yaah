package supervision

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/events"
)

// TestInFlightTable_StartEndEntries pins the table contract (yaah-k0x1): a
// Start appears with its age at a given time, oldest first; an End removes
// the matching ID; nothing open reports nil; and an End without its start
// is a no-op.
func TestInFlightTable_StartEndEntries(t *testing.T) {
	tbl := newInFlightTable(func() time.Time { return time.Unix(1000, 0) })
	if got := tbl.Entries(time.Unix(1001, 0)); got != nil {
		t.Fatalf("an empty table must report nil, got %+v", got)
	}

	tbl.Start(1, "bash", `{"command":"go test"}`, time.Unix(1000, 0))
	tbl.Start(2, "edit", `{"filePath":"a.go"}`, time.Unix(1001, 0))

	got := tbl.Entries(time.Unix(1002, 0))
	if len(got) != 2 {
		t.Fatalf("Entries = %d, want the two open calls", len(got))
	}
	if got[0].Name != "bash" || got[0].Age != 2*time.Second {
		t.Errorf("Entries[0] = {%s, age %v}, want bash aged 2s (oldest first)", got[0].Name, got[0].Age)
	}
	if got[1].Name != "edit" || got[1].Age != 1*time.Second {
		t.Errorf("Entries[1] = {%s, age %v}, want edit aged 1s", got[1].Name, got[1].Age)
	}

	tbl.End(1)
	got = tbl.Entries(time.Unix(1002, 0))
	if len(got) != 1 || got[0].Name != "edit" {
		t.Fatalf("Entries after End = %+v, want only edit", got)
	}
	tbl.End(999) // an end without its start is a no-op
	if got := tbl.Entries(time.Unix(1002, 0)); len(got) != 1 {
		t.Errorf("a bogus End must change nothing, got %+v", got)
	}
	tbl.End(2)
	if got := tbl.Entries(time.Unix(1002, 0)); got != nil {
		t.Errorf("a drained table must report nil, got %+v", got)
	}
}

// TestInFlightTable_ArgsAreClippedAndAgeClamped pins the two bounds: args
// are clipped with the snapshot's existing args cap at Start, and an age
// read from a clock behind the start clamps to zero rather than going
// backwards.
func TestInFlightTable_ArgsAreClippedAndAgeClamped(t *testing.T) {
	tbl := newInFlightTable(func() time.Time { return time.Unix(1000, 0) })
	tbl.Start(1, "bash", strings.Repeat("x", maxSnapshotArgsBytes+80), time.Unix(1000, 0))
	got := tbl.Entries(time.Unix(999, 0)) // a clock behind the start
	if len(got) != 1 {
		t.Fatalf("Entries = %d, want the open call", len(got))
	}
	if got[0].Age != 0 {
		t.Errorf("Age behind the start = %v, want clamped to 0", got[0].Age)
	}
	if len(got[0].Args) > maxSnapshotArgsBytes+3 || !strings.HasSuffix(got[0].Args, "…") {
		t.Errorf("Args = %d bytes, want the snapshot's args cap with an ellipsis tail", len(got[0].Args))
	}
}

// TestInFlightTable_ObserveFeedsStartAndEnd pins the event forwarding: a
// ToolStartEvent opens a call, the matching ToolEndEvent closes it, and
// every other event type passes through untouched.
func TestInFlightTable_ObserveFeedsStartAndEnd(t *testing.T) {
	tbl := newInFlightTable(func() time.Time { return time.Unix(1000, 0) })
	tbl.Observe(&events.ToolStartEvent{ID: 7, CallID: "c1", Name: "edit", Args: `{"filePath":"a.go"}`})
	got := tbl.Entries(time.Unix(1001, 0))
	if len(got) != 1 || got[0].Name != "edit" {
		t.Fatalf("after a start event, Entries = %+v, want the edit call", got)
	}
	// Other event types — and another call's end — pass through untouched.
	tbl.Observe(&events.TokenDeltaEvent{Text: "working"})
	tbl.Observe(&events.ToolEndEvent{ID: 6})
	if got := tbl.Entries(time.Unix(1001, 0)); len(got) != 1 {
		t.Fatalf("unrelated events must not touch the table, got %+v", got)
	}
	tbl.Observe(&events.ToolEndEvent{ID: 7})
	if got := tbl.Entries(time.Unix(1001, 0)); got != nil {
		t.Errorf("after the matching end event, Entries = %+v, want nil", got)
	}
}

// TestInFlightTable_ConcurrentAccess pins the table under concurrency:
// several goroutines Start, End, Entries, and Observe one table at once —
// no panic, no race — and after every worker has drained its calls the
// table is empty: every started-and-ended call is gone. Run under the
// race detector.
func TestInFlightTable_ConcurrentAccess(t *testing.T) {
	tbl := newInFlightTable(nil)
	const workers = 8
	const callsPerWorker = 100

	panics := make(chan string, workers+2)
	var workerWG, readerWG sync.WaitGroup
	runGuarded := func(name string, wg *sync.WaitGroup, body func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- name + ": " + fmt.Sprint(r)
				}
			}()
			body()
		}()
	}

	// Readers hammer Entries for the whole window, the way a snapshot
	// poll races the forwarder goroutine.
	stop := make(chan struct{})
	for i := 0; i < 2; i++ {
		runGuarded(fmt.Sprintf("entries-%d", i), &readerWG, func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = tbl.Entries(time.Now())
			}
		})
	}

	for w := 0; w < workers; w++ {
		runGuarded(fmt.Sprintf("worker-%d", w), &workerWG, func() {
			for i := 0; i < callsPerWorker; i++ {
				id := int64(w*callsPerWorker + i)
				tbl.Start(id, "bash", `{"command":"go test"}`, time.Now())
				tbl.End(id)
				// The same round trip through the event seam.
				tbl.Observe(&events.ToolStartEvent{ID: id + 1_000_000, Name: "edit", Args: `{"filePath":"a.go"}`})
				tbl.Observe(&events.ToolEndEvent{ID: id + 1_000_000})
			}
		})
	}

	workerWG.Wait()
	close(stop)
	readerWG.Wait()
	close(panics)

	if got := tbl.Entries(time.Now()); got != nil {
		t.Errorf("Entries after every call ended = %+v, want nil — every started-and-ended call must be gone", got)
	}
	for msg := range panics {
		t.Errorf("goroutine panicked: %s", msg)
	}
}

// TestInFlightTable_NilClockFallsBackToRealClock pins newInFlightTable's
// fallback: a table built with a nil clock still stamps starts with a real
// one — Observe must not panic on the nil field, and the age it reports is
// a small real elapsed time, not the epoch a zero stamp would give.
func TestInFlightTable_NilClockFallsBackToRealClock(t *testing.T) {
	tbl := newInFlightTable(nil)
	tbl.Observe(&events.ToolStartEvent{ID: 1, Name: "bash", Args: "{}"})
	got := tbl.Entries(time.Now())
	if len(got) != 1 {
		t.Fatalf("Entries = %+v, want the observed start", got)
	}
	if got[0].Age < 0 || got[0].Age > time.Minute {
		t.Errorf("Age = %v, want a small real duration — the nil clock must fall back to a real one", got[0].Age)
	}
}
