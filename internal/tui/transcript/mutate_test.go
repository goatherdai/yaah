package transcript

import (
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// TestSweepRunningTools pins the turn-end seal: every still-Running tool
// block becomes Interrupted, completed and failed blocks keep their
// states, and the sweep reports how many it sealed.
func TestSweepRunningTools(t *testing.T) {
	th := colors.NewDarkTheme()
	m := NewModel(&th)
	running := toolblock.New("r1", "bash", "{}", &th)
	done := toolblock.New("d1", "read", "{}", &th)
	failed := toolblock.New("f1", "grep", "{}", &th)
	m.Append(ToolItem(running))
	m.Append(ToolItem(done))
	m.Append(ToolItem(failed))
	m.SettleTool("d1", ToolEnd{Summary: "ok", Result: "result"})
	m.SettleTool("f1", ToolEnd{Summary: "boom", Result: "error", Error: true})

	m.Repaint(80) // establish the painted row table
	if n := len(m.SweepRunningTools()); n != 1 {
		t.Fatalf("sweep sealed %d blocks, want 1", n)
	}
	if running.S() != toolblock.Interrupted {
		t.Errorf("running block state = %d, want Interrupted", running.S())
	}
	if done.S() != toolblock.Done {
		t.Errorf("done block was disturbed by the sweep")
	}
	if failed.S() != toolblock.Error {
		t.Errorf("failed block was disturbed by the sweep")
	}

	// A second sweep is a no-op: the sealed blocks are terminal now.
	if n := len(m.SweepRunningTools()); n != 0 {
		t.Fatalf("second sweep sealed %d blocks, want 0", n)
	}
}

// TestSweepRunningTools_ReportsTheSealedKeys pins that the sweep names what it
// sealed, so a caller can attribute each block to its own cause instead of
// treating every sweep as one undifferentiated loss.
func TestSweepRunningTools_ReportsTheSealedKeys(t *testing.T) {
	th := colors.NewDarkTheme()
	m := NewModel(&th)
	m.Append(ToolItem(toolblock.New("run1.1", "bash", "{}", &th)))
	m.Append(ToolItem(toolblock.New("run1.2", "read", "{}", &th)))
	m.SettleTool("run1.2", ToolEnd{Summary: "ok", Result: "result"})

	sealed := m.SweepRunningTools()
	if len(sealed) != 1 || sealed[0] != "run1.1" {
		t.Fatalf("sweep sealed %v, want exactly [run1.1]", sealed)
	}
}

// TestSweepRunningTools_MarksDirty pins that sealing invalidates the
// swept block's painted rows: the next repaint renders it terminal with
// the interrupted marker instead of the Running chevron.
func TestSweepRunningTools_MarksDirty(t *testing.T) {
	th := colors.NewDarkTheme()
	m := NewModel(&th)
	m.Append(ToolItem(toolblock.New("r1", "bash", "{}", &th)))
	m.Repaint(80)
	m.SweepRunningTools()
	m.Repaint(80) // the refresh the dirty flag schedules in production
	if !strings.Contains(m.PaintedText(), "◌") {
		t.Fatalf("swept block not repainted terminal: %q", m.PaintedText())
	}
	if strings.Contains(m.PaintedText(), "▶") {
		t.Fatalf("swept block still painted with the running chevron: %q", m.PaintedText())
	}
}
