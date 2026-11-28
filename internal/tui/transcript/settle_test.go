package transcript

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// settle_test.go — the single settlement rule every tool end event goes
// through (yaah-77f1). A tool call is identified by a session-unique key, so
// a later run's call can never settle an earlier run's block; what remains
// is what to do when the event meets no Running block at all.

// settleFixture builds a model holding one tool block per given id, in order.
func settleFixture(th *colors.Theme, ids ...string) (*Model, []*toolblock.Block) {
	m := NewModel(th)
	blocks := make([]*toolblock.Block, 0, len(ids))
	for _, id := range ids {
		b := toolblock.New(id, "bash", "{}", th)
		m.Append(ToolItem(b))
		blocks = append(blocks, b)
	}
	return m, blocks
}

// TestSettleTool pins the state machine: Running -> Done | Error |
// Interrupted, and an end event is applied by the rule in SettleTool's doc —
// last Running block wins, an Interrupted block heals, an already-terminal
// block is untouched, and nothing matched is stashed.
func TestSettleTool(t *testing.T) {
	th := colors.NewDarkTheme()

	tests := []struct {
		name string
		// ids are the blocks in transcript order; state pre-settles a block
		// before the event under test.
		ids     []string
		settle  map[string]ToolEnd // ends applied before the event under test
		key     string
		end     ToolEnd
		want    Settlement
		states  []toolblock.State
		stashed int
	}{
		{
			name:   "running block settles",
			ids:    []string{"k.1"},
			key:    "k.1",
			end:    ToolEnd{Summary: "ok", Result: "out"},
			want:   Settled,
			states: []toolblock.State{toolblock.Done},
		},
		{
			name:   "error settles a running block as error",
			ids:    []string{"k.1"},
			key:    "k.1",
			end:    ToolEnd{Summary: "boom", Result: "denied", Error: true},
			want:   Settled,
			states: []toolblock.State{toolblock.Error},
		},
		{
			// The defect: one key, two blocks, because a later run reused the
			// earlier run's ordinal. The live (last) block is the one that
			// settles; the older one keeps what it had.
			name:    "last running block wins",
			ids:     []string{"k.1", "k.1"},
			settle:  map[string]ToolEnd{},
			key:     "k.1",
			end:     ToolEnd{Summary: "newer", Result: "newer result"},
			want:    Settled,
			states:  []toolblock.State{toolblock.Running, toolblock.Done},
			stashed: 0,
		},
		{
			name:   "duplicate end does not overwrite",
			ids:    []string{"k.1"},
			settle: map[string]ToolEnd{"k.1": {Summary: "first", Result: "first result"}},
			key:    "k.1",
			end:    ToolEnd{Summary: "second", Result: "second result"},
			want:   DuplicateEnd,
			states: []toolblock.State{toolblock.Done},
		},
		{
			name:   "error after done does not flip the state",
			ids:    []string{"k.1"},
			settle: map[string]ToolEnd{"k.1": {Summary: "first", Result: "first result"}},
			key:    "k.1",
			end:    ToolEnd{Summary: "late failure", Result: "boom", Error: true},
			want:   DuplicateEnd,
			states: []toolblock.State{toolblock.Done},
		},
		{
			name: "late end heals an interrupted block",
			ids:  []string{"k.1"},
			// No pre-settle hook here: Interrupted is what the turn-end
			// sweep leaves behind, so the fixture interrupts it directly.
			key:    "k.1",
			end:    ToolEnd{Summary: "ok", Result: "out", Duration: 250 * time.Millisecond},
			want:   LateEnd,
			states: []toolblock.State{toolblock.Interrupted}, // replaced below
		},
		{
			name:    "unmatched end is stashed",
			ids:     []string{"k.1"},
			key:     "other.9",
			end:     ToolEnd{Summary: "ok", Result: "out"},
			want:    UnmatchedEnd,
			states:  []toolblock.State{toolblock.Running},
			stashed: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, blocks := settleFixture(&th, tc.ids...)
			if tc.name == "late end heals an interrupted block" {
				blocks[0].Interrupt()
				tc.states = []toolblock.State{toolblock.Done}
			}
			for key, end := range tc.settle {
				m.SettleTool(key, end)
			}

			if got := m.SettleTool(tc.key, tc.end); got != tc.want {
				t.Errorf("SettleTool = %v, want %v", got, tc.want)
			}
			for i, b := range blocks {
				if b.S() != tc.states[i] {
					t.Errorf("block %d state = %v, want %v", i, b.S(), tc.states[i])
				}
			}
			if got := len(m.UnmatchedEnds()); got != tc.stashed {
				t.Errorf("stashed ends = %d, want %d", got, tc.stashed)
			}
		})
	}
}

// TestSettleTool_ReusedKeyLeavesTheOlderBlockByteIdentical pins the property
// the whole identity change exists for: when a later run's call arrives, the
// earlier block's tag, result and duration are untouched — not exchanged with
// the newer call's result (yaah-77f1).
func TestSettleTool_ReusedKeyLeavesTheOlderBlockByteIdentical(t *testing.T) {
	th := colors.NewDarkTheme()
	m, blocks := settleFixture(&th, "run1.1", "run2.1")
	older, newer := blocks[0], blocks[1]

	// The first run's call settles normally.
	if got := m.SettleTool("run1.1", ToolEnd{Summary: "first", Result: "first result", Duration: time.Second}); got != Settled {
		t.Fatalf("first settlement = %v, want Settled", got)
	}
	before := older.RenderCtx(colors.RenderCtx{Theme: &th, Width: 80})

	// Now a second run's call, whose block was keyed identically under the
	// old counter identity, settles.
	if got := m.SettleTool("run1.1", ToolEnd{Summary: "second", Result: "second result", Duration: 2 * time.Second}); got != DuplicateEnd {
		t.Fatalf("end for a settled key = %v, want DuplicateEnd", got)
	}
	if newer.S() != toolblock.Running {
		t.Errorf("newer block state = %v, want Running (it was keyed differently)", newer.S())
	}
	if after := older.RenderCtx(colors.RenderCtx{Theme: &th, Width: 80}); after != before {
		t.Errorf("the older block changed:\n before %q\n  after %q", before, after)
	}
}

// TestSettleTool_LateEndUsesTheEventDuration pins the label honesty of a
// healed block: it reports how long the call ran, not how late its event
// arrived.
func TestSettleTool_LateEndUsesTheEventDuration(t *testing.T) {
	th := colors.NewDarkTheme()
	m, blocks := settleFixture(&th, "run1.1")
	block := blocks[0]
	block.Interrupt()

	if got := m.SettleTool("run1.1", ToolEnd{Summary: "ok", Result: "out", Duration: 250 * time.Millisecond}); got != LateEnd {
		t.Fatalf("SettleTool on an interrupted block = %v, want LateEnd", got)
	}
	if block.S() != toolblock.Done {
		t.Fatalf("healed block state = %v, want Done", block.S())
	}
	rendered := block.RenderCtx(colors.RenderCtx{Theme: &th, Width: 80})
	if !strings.Contains(rendered, "250ms") {
		t.Errorf("healed block duration = %q, want the event's 250ms", rendered)
	}
}

// TestSettleTool_UnmatchedEndsAreBounded pins that the diagnostic stash
// cannot grow without bound: the newest ends are kept, the oldest dropped.
func TestSettleTool_UnmatchedEndsAreBounded(t *testing.T) {
	th := colors.NewDarkTheme()
	m, _ := settleFixture(&th, "run1.1")

	last := ""
	for i := range unmatchedEndsLimit + 4 {
		last = "gone." + strconv.Itoa(i)
		if got := m.SettleTool(last, ToolEnd{Summary: last}); got != UnmatchedEnd {
			t.Fatalf("SettleTool(%q) = %v, want UnmatchedEnd", last, got)
		}
	}
	stashed := m.UnmatchedEnds()
	if len(stashed) != unmatchedEndsLimit {
		t.Fatalf("stashed %d ends, want the ring capped at %d", len(stashed), unmatchedEndsLimit)
	}
	if stashed[len(stashed)-1].Key != last {
		t.Errorf("newest stashed key = %q, want the last one offered (%q)", stashed[len(stashed)-1].Key, last)
	}
}

// TestSettleTool_SettledBlockRepaintsItsRows pins that a settlement
// invalidates the block's painted rows; without that the transcript keeps
// rendering the running chevron.
func TestSettleTool_SettledBlockRepaintsItsRows(t *testing.T) {
	th := colors.NewDarkTheme()
	m, _ := settleFixture(&th, "run1.1")
	m.Repaint(80)

	m.SettleTool("run1.1", ToolEnd{Summary: "ok", Result: "out"})
	m.Repaint(80) // the refresh the dirty flag schedules in production

	if strings.Contains(m.PaintedText(), "▶") {
		t.Errorf("settled block still painted as running: %q", m.PaintedText())
	}
	if !strings.Contains(m.PaintedText(), "✓") {
		t.Errorf("settled block not painted as done: %q", m.PaintedText())
	}
}
