package rule

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func newSim(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(w, h)
	return sim
}

func TestDrawPaintsSeparator(t *testing.T) {
	sim := newSim(t, 10, 1)
	r := New("#888888")
	r.SetRect(0, 0, 10, 1)
	r.Draw(sim)
	sim.Show()
	cells, _, _ := sim.GetContents()
	for i, c := range cells {
		if len(c.Runes) == 0 || c.Runes[0] != '─' {
			t.Fatalf("cell %d painted %q, want a separator rune", i, c.Runes)
		}
	}
}

func TestDrawEmptyRect(t *testing.T) {
	sim := newSim(t, 10, 1)
	r := New("#888888")
	r.SetRect(0, 0, 0, 0)
	r.Draw(sim)
	sim.Show()
	cells, _, _ := sim.GetContents()
	for i, c := range cells {
		if len(c.Runes) != 0 {
			t.Fatalf("cell %d painted %q on an empty rect", i, c.Runes)
		}
	}
}
