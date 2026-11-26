package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// This file is the yaah-30t6 T0 gate. It builds a synthetic session at the
// scale that froze the TUI (issue acceptance criterion 4: ~175 turns, ~1M
// cumulative prompt tokens ≈ ~4MB of painted transcript) and asserts two
// properties: a full render pass stays inside a wall-clock budget, and
// keystrokes are echoed while heavy renders are queued on the tview event
// loop. The budgets are deliberately generous — the gate exists to catch
// the pathological O(transcript)-per-event scaling, not to measure
// hardware. Tighten them when the T2 windowed painter lands.

// synthProse returns deterministic markdown-ish text of at least target
// bytes: long single-line paragraphs to exercise wrap cost, with hard breaks
// between paragraphs to exercise the line index.
func synthProse(marker string, target int) string {
	sentence := "The renderer must stay responsive while the transcript grows, " +
		"so this paragraph exercises the wrap cost of long streamed markdown. "
	var b strings.Builder
	for b.Len() < target {
		fmt.Fprintf(&b, "%s: ", marker)
		for i := 0; i < 8 && b.Len() < target; i++ {
			b.WriteString(sentence)
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

// setupSim lays the app out on a 120x40 simulation screen, the same shape
// the append_render tests use, so messageWidth and Draw see a real layout.
func setupSim(tb testing.TB, ui *App) tcell.SimulationScreen {
	tb.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		tb.Fatalf("init simulation screen: %v", err)
	}
	tb.Cleanup(func() { sim.Fini() })
	sim.SetSize(120, 40)
	ui.Root.SetRect(0, 0, 120, 40)
	ui.Root.Draw(sim)
	return sim
}

// growSession appends one assistant turn in the same shape a live session
// produces (prompt, occasional reasoning, markdown response, two tool calls)
// and refreshes once per turn, mimicking the per-event refresh cadence of a
// streamed run. ~28KB of content per turn.
func growSession(tb testing.TB, ui *App, turns int) {
	tb.Helper()
	for i := 0; i < turns; i++ {
		ui.appendMessage(fmt.Sprintf("turn %d: investigate the render cost", i))
		if i%5 == 0 {
			ui.AddReasoningBlock(fmt.Sprintf("r-%d", i), synthProse(fmt.Sprintf("reasoning %d", i), 2048))
		}
		ui.addAssistantResponse(synthProse(fmt.Sprintf("response %d", i), 12*1024))
		for j := 0; j < 2; j++ {
			id := fmt.Sprintf("%d-%d", i, j)
			ui.AddToolStart(id, "bash", `{"command":"go test ./..."}`)
			ui.AddToolEnd(id, "bash · go test", synthProse(fmt.Sprintf("tool %s", id), 8*1024))
		}
		ui.refreshMessages()
	}
}

// TestRefreshMessages_SessionScaleRenderBudget pins the per-event render
// cost at the end of a full-scale session: one full render plus one screen
// draw must stay inside the budget, and a tool-chunk append must stay far
// below it (the incremental path T2 extends to text chunks).
func TestRefreshMessages_SessionScaleRenderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("full-scale render gate: skipped in -short")
	}
	ui := New("test")
	sim := setupSim(t, ui)

	buildStart := time.Now()
	growSession(t, ui, 175)
	build := time.Since(buildStart)

	// Same-goroutine read: every mutation and refresh so far ran on this
	// goroutine, so there is no race with the TextView buffer.
	rendered := ui.Messages.GetText(true)
	if !strings.Contains(rendered, "turn 0: investigate") ||
		!strings.Contains(rendered, "turn 174: investigate") {
		t.Fatalf("painted transcript lost early or late turns (%d bytes)", len(rendered))
	}

	// Steady state: the cost a user pays per event at this scale.
	start := time.Now()
	ui.refreshMessages()
	ui.Root.Draw(sim)
	fullRender := time.Since(start)

	// The incremental path: one new tool chunk, no in-place mutation.
	before := ui.fullRenderCount
	ui.AddToolStart("tail", "bash", `{"command":"true"}`)
	start = time.Now()
	ui.refreshMessages()
	ui.Root.Draw(sim)
	appendRender := time.Since(start)
	if ui.fullRenderCount != before {
		t.Fatalf("tool append took the full-render path (%d -> %d)", before, ui.fullRenderCount)
	}

	t.Logf("built 175 turns (%d items) in %s; full render+draw %s; append render+draw %s",
		len(ui.conversationLog), build.Round(time.Millisecond),
		fullRender.Round(time.Millisecond), appendRender.Round(time.Millisecond))

	const fullBudget = 3 * time.Second
	const appendBudget = 300 * time.Millisecond
	if fullRender > fullBudget {
		t.Errorf("full render+draw at session scale took %s, budget %s — the yaah-30t6 freeze shape", fullRender, fullBudget)
	}
	if appendRender > appendBudget {
		t.Errorf("append render+draw took %s, budget %s", appendRender, appendBudget)
	}
}

// TestInputEchoDuringRenderStorm runs the real tview event loop on the
// simulation screen, queues a burst of full renders the way live events do
// (mutations inside QueueUpdateDraw), and injects keystrokes while they
// drain. Every keystroke must be echoed within the budget: this is the
// "must remain typable" acceptance criterion, measured at the loop, where
// the freeze happens.
func TestInputEchoDuringRenderStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("render-storm gate: skipped in -short")
	}
	ui := New("test")
	sim := setupSim(t, ui)
	growSession(t, ui, 60)

	echoed := make(chan struct{}, 16)
	prev := ui.App.GetInputCapture()
	ui.App.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEnter {
			echoed <- struct{}{}
			return nil
		}
		return prev(ev)
	})

	ui.App.SetScreen(sim)
	go ui.App.Run()
	defer ui.App.Stop()

	const keys = 5
	go func() {
		for i := 0; i < keys; i++ {
			time.Sleep(20 * time.Millisecond)
			sim.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
		}
	}()

	var worst time.Duration
	for i := 0; i < keys; i++ {
		start := time.Now()
		select {
		case <-echoed:
			if d := time.Since(start); d > worst {
				worst = d
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("keystroke %d not echoed within 3s during a render storm (worst echo %s)", i+1, worst)
		}
	}
	t.Logf("echoed %d keystrokes during a render storm; worst latency %s", keys, worst.Round(time.Millisecond))
}

// BenchmarkRefreshMessages_SessionScale measures the per-event cost at
// session scale: a forced full render plus draw (the ToolEnd/toggle path)
// against a 175-turn transcript.
func BenchmarkRefreshMessages_SessionScale(b *testing.B) {
	if testing.Short() {
		b.Skip("full-scale benchmark: skipped in -short")
	}
	ui := New("test")
	sim := setupSim(b, ui)
	growSession(b, ui, 175)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ui.needsFullRender.Store(true)
		ui.refreshMessages()
		ui.Root.Draw(sim)
	}
}
