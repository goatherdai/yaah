package tui

import (
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// This file pins the ledger's own rules. The behaviours it makes possible are
// pinned where they are observable (tool_drop_test.go); what is checked here is
// the state machine underneath them, because every one of these rules replaced a
// review finding from the previous design (yaah-ol3r).
//
//	gen < swept gen  -> retired: no start can still arrive                 (r5)
//	gen == swept, endOrLost, start alive -> kept for a late start           (r1/r2)
//	gen == swept, start dropped -> spent, never re-added                   (r5)
//	lookup always matches (gen, key) -> a repeated ordinal cannot leak     (r2/r3)

func TestLedgerDroppedStartIsSpentAtSweep(t *testing.T) {
	l := newToolLedger()
	l.markStartDropped(1, "run1.1")
	l.markEndLost(1, "run1.1")

	entries := l.closeTurn(1)
	if e, ok := entries["run1.1"]; !ok || e.gen != 1 {
		t.Fatalf("snapshot = %+v, want the entry at gen 1 so the sweep can attribute it", entries)
	}
	if n := l.pending(); n != 0 {
		t.Fatalf("entries kept = %d, want 0: a call whose start was dropped has nothing to wait for and must not be re-added", n)
	}
}

func TestLedgerKeepsAnEndForAStartThatMayStillLand(t *testing.T) {
	l := newToolLedger()
	l.stashEnd(1, "run1.1", transcript.ToolEnd{Summary: "read", Result: "out"})
	l.closeTurn(1)
	if n := l.pending(); n != 1 {
		t.Fatalf("entries kept = %d, want 1: the sweep can run while this turn's start is still in flight", n)
	}

	loss, ok := l.takeForStart(1, "run1.1")
	if !ok || !loss.hasEnd || loss.end.Result != "out" {
		t.Fatalf("takeForStart = (%+v, %v), want the stashed end", loss, ok)
	}
	if n := l.pending(); n != 0 {
		t.Fatalf("entries after the start consumed one = %d, want 0", n)
	}
}

func TestLedgerLostEndSurvivesItsTurnSweep(t *testing.T) {
	l := newToolLedger()
	l.markEndLost(1, "run1.1")
	l.closeTurn(1)
	if _, ok := l.takeForStart(1, "run1.1"); !ok {
		t.Fatal("a dropped end must be kept for a start that may still land, or that block stays Running forever")
	}
}

func TestLedgerKeepsAnEarlierGenerationsRecordForItsPendingStart(t *testing.T) {
	l := newToolLedger()
	l.stashEnd(1, "run1.1", transcript.ToolEnd{Summary: "turn 1", Result: "one", Duration: time.Millisecond})

	// A later turn's sweep must not retire it: the critical fallback delivers
	// closures from their own goroutines, so a later sweep can arrive before an
	// earlier turn's start that is still waiting in the managed queue.
	l.closeTurn(2)
	if n := l.pending(); n != 1 {
		t.Fatalf("entries kept = %d, want 1: an earlier turn's end must survive for a start that may still be pending", n)
	}

	// The generation still separates them: turn 2 cannot claim it...
	if _, ok := l.takeForStart(2, "run1.1"); ok {
		t.Fatal("a later turn's start consumed an earlier turn's end")
	}
	// ...but turn 1's own start still can, and it is the normal settle path.
	if loss, ok := l.takeForStart(1, "run1.1"); !ok || loss.end.Result != "one" {
		t.Fatalf("takeForStart = (%+v, %v), want turn 1's end", loss, ok)
	}
}

func TestLedgerGenerationReplacesASharedFallbackKey(t *testing.T) {
	l := newToolLedger()
	l.stashEnd(1, "1", transcript.ToolEnd{Summary: "turn 1", Result: "one"})
	l.stashEnd(2, "1", transcript.ToolEnd{Summary: "turn 2", Result: "two"})

	loss, ok := l.takeForStart(2, "1")
	if !ok || loss.end.Result != "two" {
		t.Fatalf("takeForStart = (%+v, %v), want turn 2's end: a reused ordinal must not resolve to the older turn", loss, ok)
	}
}

func TestLedgerBackstopBoundsAPathologicalRun(t *testing.T) {
	l := newToolLedger()
	for i := 0; i < ledgerBackstop+5; i++ {
		l.markEndLost(1, "run1."+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if n := l.pending(); n > ledgerBackstop {
		t.Fatalf("entries = %d, want at most the %d backstop", n, ledgerBackstop)
	}
}

func TestLedgerForgetDropsEverything(t *testing.T) {
	l := newToolLedger()
	l.stashEnd(1, "run1.1", transcript.ToolEnd{Summary: "read", Result: "out"})
	l.forget()
	if n := l.pending(); n != 0 {
		t.Fatalf("entries after forget = %d, want 0", n)
	}
}
