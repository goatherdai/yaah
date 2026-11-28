package transcript

import (
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
// block is untouched, and nothing matched leaves no record in the model.
func TestSettleTool(t *testing.T) {
	th := colors.NewDarkTheme()

	tests := []struct {
		name string
		// ids are the blocks in transcript order; state pre-settles a block
		// before the event under test.
		ids    []string
		settle map[string]ToolEnd // ends applied before the event under test
		key    string
		end    ToolEnd
		want   Settlement
		states []toolblock.State
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
			name:   "last running block wins",
			ids:    []string{"k.1", "k.1"},
			settle: map[string]ToolEnd{},
			key:    "k.1",
			end:    ToolEnd{Summary: "newer", Result: "newer result"},
			want:   Settled,
			states: []toolblock.State{toolblock.Running, toolblock.Done},
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
			// The model keeps no copy of an unmatched end: the view records
			// it against its turn so a late start can inherit it
			// (internal/tui/tool_ledger.go). The block is untouched.
			name:   "unmatched end settles nothing",
			ids:    []string{"k.1"},
			key:    "other.9",
			end:    ToolEnd{Summary: "ok", Result: "out"},
			want:   UnmatchedEnd,
			states: []toolblock.State{toolblock.Running},
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
		})
	}
}

// TestSettleTool_ReusedKeySettlesEachBlockInTurn pins the property the whole
// identity change exists for: under the old counter identity a later run's
// call reused a key, so two blocks shared one. Each end must still land on its
// own block — the later run's end settles the LAST Running match, the earlier
// run's own end settles the one before it, and neither block's tag, result or
// duration is exchanged for the other's (yaah-77f1).
func TestSettleTool_ReusedKeySettlesEachBlockInTurn(t *testing.T) {
	th := colors.NewDarkTheme()
	m, blocks := settleFixture(&th, "run1.1", "run1.1")
	older, newer := blocks[0], blocks[1]
	rc := colors.RenderCtx{Theme: &th, Width: 80}

	// The second run's end arrives while both blocks are Running: the last
	// Running block with the key is the newer one.
	if got := m.SettleTool("run1.1", ToolEnd{Summary: "second", Result: "second result", Duration: 2 * time.Second}); got != Settled {
		t.Fatalf("end for the newer block = %v, want Settled", got)
	}
	if newer.S() != toolblock.Done {
		t.Fatalf("newer block state = %v, want Done", newer.S())
	}
	if older.S() != toolblock.Running {
		t.Fatalf("older block state = %v, want Running: the newer run's end reached it", older.S())
	}

	// The first run's own end then settles what is still Running — the older
	// block — even though its wall-clock time is later.
	if got := m.SettleTool("run1.1", ToolEnd{Summary: "first", Result: "first result", Duration: time.Second}); got != Settled {
		t.Fatalf("end for the older block = %v, want Settled", got)
	}

	olderAfter, newerAfter := older.RenderCtx(rc), newer.RenderCtx(rc)
	if !strings.Contains(olderAfter, "first") || strings.Contains(olderAfter, "second") {
		t.Errorf("older block rendered %q, want only its own tag", olderAfter)
	}
	if !strings.Contains(newerAfter, "second") || strings.Contains(newerAfter, "first") {
		t.Errorf("newer block rendered %q, want only its own tag", newerAfter)
	}
	if !strings.Contains(olderAfter, "1.0s") {
		t.Errorf("older block rendered %q, want its own 1.0s duration", olderAfter)
	}
	if !strings.Contains(newerAfter, "2.0s") {
		t.Errorf("newer block rendered %q, want its own 2.0s duration", newerAfter)
	}

	// With nothing Running, a further end for the same key is a duplicate and
	// changes neither block.
	if got := m.SettleTool("run1.1", ToolEnd{Summary: "third", Result: "third result", Duration: 3 * time.Second}); got != DuplicateEnd {
		t.Fatalf("end with no Running block = %v, want DuplicateEnd", got)
	}
	if got := older.RenderCtx(rc); got != olderAfter {
		t.Errorf("a duplicate end changed the older block:\n before %q\n  after %q", olderAfter, got)
	}
	if got := newer.RenderCtx(rc); got != newerAfter {
		t.Errorf("a duplicate end changed the newer block:\n before %q\n  after %q", newerAfter, got)
	}

	// Expanded, each block still reports its own call's result: the settlement
	// exchanged nothing between the two blocks that shared a key.
	older.Toggle()
	newer.Toggle()
	olderExp, newerExp := older.RenderCtx(rc), newer.RenderCtx(rc)
	if !strings.Contains(olderExp, "first result") || strings.Contains(olderExp, "second result") {
		t.Errorf("expanded older block = %q, want only its own result", olderExp)
	}
	if !strings.Contains(newerExp, "second result") || strings.Contains(newerExp, "first result") {
		t.Errorf("expanded newer block = %q, want only its own result", newerExp)
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
