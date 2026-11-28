package tui

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// This file is the yaah-30t6 T0 gate. It builds a synthetic session at the
// scale that froze the TUI (issue acceptance criterion 4: ~175 turns, ~1M
// cumulative prompt tokens ≈ ~4MB of painted transcript) and asserts two
// properties: a full render pass stays proportional to a small-session
// baseline measured on the same machine, and keystrokes are echoed while
// heavy renders are queued on the tview event loop. The gate exists to
// catch the pathological O(transcript)-per-event scaling, not to measure
// hardware. The budgets are tightened post-T2: the windowed painter holds
// the session-scale rebuild at ~1x the baseline.

// TestMain isolates the whole package from the developer's real yaah
// home: any test whose refresh crosses the SLOW threshold would otherwise
// create ~/.yaah and append to the real tui-render.log — the artifact
// users are meant to attach to bug reports. Tests that assert log-file
// contents set YAAH_HOME themselves via t.Setenv, which overrides this.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "yaah-tui-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("YAAH_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

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
// the append_render tests use, so the pane's width and Draw see a real
// layout.
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
// without rendering per turn: the transcript is seeded in O(n) so the
// default CI job pays no O(n²) setup cost, and the measured refresh paints
// everything in one full-render pass. ~28KB of content per turn.
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
			ui.AddToolEnd(id, "bash · go test", synthProse(fmt.Sprintf("tool %s", id), 8*1024), 0)
		}
	}
}

// renderGateFactor is the YAAH_TEST_RENDER_BUDGET multiplier (default 1):
// a manual escape hatch for slow or loaded machines. CI relies on the
// ratio-based budgets, which cancel machine speed out; the factor exists
// so a pathological environment can widen everything without a code
// change.
func renderGateFactor(tb testing.TB) float64 {
	tb.Helper()
	if f := os.Getenv("YAAH_TEST_RENDER_BUDGET"); f != "" {
		if v, err := strconv.ParseFloat(f, 64); err == nil && v > 0 {
			return v
		}
	}
	return 1
}

// renderGateBudget scales a wall-clock budget by the factor above.
func renderGateBudget(tb testing.TB, base time.Duration) time.Duration {
	tb.Helper()
	return time.Duration(float64(base) * renderGateFactor(tb))
}

// gateApp returns an app with an undrained queue: mutators' markDirty
// enqueues exactly one event (the flags then stay set, so nothing more is
// enqueued) instead of spawning direct-fallback goroutines that would
// block forever on an app that never runs. Every refresh in these tests
// is driven directly, on the test goroutine.
func gateApp() *App {
	ui := New("test")
	ui.bgDone = make(chan struct{})
	ui.uiEventCh = make(chan uiEvent, 512)
	return ui
}

// measureRender runs one refresh plus screen draw — the per-event cost a
// user pays — and returns the duration.
func measureRender(ui *App, sim tcell.SimulationScreen) time.Duration {
	start := time.Now()
	ui.refreshMessages()
	ui.Root.Draw(sim)
	return time.Since(start)
}

// measureBest returns the fastest of runs refresh+draw passes after one
// untimed warmup pass. The minimum is the cleanest signal of the work's
// true cost: scheduler hiccups and GC pauses only ever inflate a sample.
func measureBest(ui *App, sim tcell.SimulationScreen, runs int) time.Duration {
	measureRender(ui, sim)
	best := time.Duration(math.MaxInt64)
	for i := 0; i < runs; i++ {
		if d := measureRender(ui, sim); d < best {
			best = d
		}
	}
	return best
}

// resizeSim re-lays the app out at a different width: the next repaint
// misses every width-keyed cache (markdown, wrapped rows), which is the
// cold path a terminal resize hits at session scale.
func resizeSim(ui *App, sim tcell.SimulationScreen) {
	sim.SetSize(100, 40)
	ui.Root.SetRect(0, 0, 100, 40)
	ui.Root.Draw(sim)
}

// TestRefreshMessages_SessionScaleRenderBudget pins the per-event render
// cost at the end of a full-scale session. The budget is a ratio against
// a small-session baseline measured in the same process on the same
// machine, so runner speed cancels out and the gate keeps its signal in
// CI (a wall-clock budget scaled 3x for hosted runners would only catch
// pathological regressions). Both the warm-cache rebuild and the
// cold-cache resize rebuild — the expensive path when cachedWidth no
// longer matches — are gated.
func TestRefreshMessages_SessionScaleRenderBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("full-scale render gate: skipped in -short")
	}

	base := gateApp()
	baseSim := setupSim(t, base)
	growSession(t, base, 18)
	baseWarm := measureBest(base, baseSim, 3)
	resizeSim(base, baseSim)
	baseCold := measureBest(base, baseSim, 3)

	ui := gateApp()
	sim := setupSim(t, ui)
	buildStart := time.Now()
	growSession(t, ui, 175)
	build := time.Since(buildStart)

	warm := measureBest(ui, sim, 3)

	// Same-goroutine read: every mutation and refresh so far ran on this
	// goroutine, so there is no race with the TextView buffer. T2's
	// windowed painter: the late turns must be painted, the early ones
	// must be collapsed behind the seam, and the buffer must be bounded
	// by the window budget instead of the transcript size.
	rendered := ui.tr.PaintedText()
	if !strings.Contains(rendered, "turn 174: investigate") {
		t.Fatalf("painted transcript lost the late turns (%d bytes)", len(rendered))
	}
	if strings.Contains(rendered, "turn 0: investigate") {
		t.Fatalf("early turns painted at session scale: the window did not engage (%d bytes)", len(rendered))
	}
	if !strings.Contains(rendered, "earlier messages") {
		t.Fatalf("window seam missing at session scale (%d bytes)", len(rendered))
	}

	// The incremental path: one new tool chunk, no in-place mutation.
	before := ui.tr.FullRenders()
	ui.AddToolStart("tail", "bash", `{"command":"true"}`)
	appendRender := measureRender(ui, sim)
	if ui.tr.FullRenders() != before {
		t.Fatalf("tool append took the full-render path (%d -> %d)", before, ui.tr.FullRenders())
	}

	resizeSim(ui, sim)
	cold := measureBest(ui, sim, 3)

	t.Logf("built 175 turns (%d items) in %s", ui.tr.Len(), build.Round(time.Millisecond))
	t.Logf("full render+draw: warm %s (base %s, ratio %.1f); cold %s (base %s, ratio %.1f); append %s",
		warm.Round(time.Millisecond), baseWarm.Round(time.Millisecond),
		float64(warm)/float64(baseWarm),
		cold.Round(time.Millisecond), baseCold.Round(time.Millisecond),
		float64(cold)/float64(baseCold),
		appendRender.Round(time.Millisecond))

	// The ratio budget is tight now: the T2 windowed painter bounds the
	// full render by the window budget (renderWindowItems/Rows), so the
	// 175-turn session rebuild measures ~1.0x the 18-turn baseline
	// (windowed warm and cold). A superlinear regression — the window
	// budget failing to bind — trips 4x with room for runner jitter,
	// which moves both measurements together.
	ratio := 4.0 * renderGateFactor(t)
	gate := func(base time.Duration) time.Duration {
		d := time.Duration(float64(base) * ratio)
		if floor := renderGateBudget(t, 250*time.Millisecond); floor > d {
			return floor
		}
		return d
	}
	if warm > gate(baseWarm) {
		t.Errorf("warm full render+draw took %s, %.1fx the small-session baseline %s (budget %.0fx) — the yaah-30t6 freeze shape",
			warm, float64(warm)/float64(baseWarm), baseWarm, ratio)
	}
	if cold > gate(baseCold) {
		t.Errorf("cold (resize) full render+draw took %s, %.1fx the small-session baseline %s (budget %.0fx)",
			cold, float64(cold)/float64(baseCold), baseCold, ratio)
	}
	if appendRender > renderGateBudget(t, 300*time.Millisecond) {
		t.Errorf("append render+draw took %s, budget %s", appendRender, renderGateBudget(t, 300*time.Millisecond))
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

	// One measured full render calibrates the echo budget: each injected
	// keystroke may wait behind `renders` queued renders, so the budget is
	// that backlog plus slack — not a constant a slow runner could breach
	// while behaving correctly.
	ui.tr.RequestFull()
	oneRender := measureRender(ui, sim)

	echoed := make(chan struct{}, 16)
	prev := ui.App.GetInputCapture()
	ui.App.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEnter {
			echoed <- struct{}{}
			return nil
		}
		return prev(ev)
	})

	// Cancellation for the storm and injector goroutines, so a Fatalf
	// cannot leave them running into sim.Fini (cleanups are LIFO: this
	// runs before the screen is finalized).
	quit := make(chan struct{})
	t.Cleanup(func() { close(quit) })

	ui.App.SetScreen(sim)
	runErr := make(chan error, 1)
	go func() { runErr <- ui.App.Run() }()
	t.Cleanup(func() {
		ui.App.Stop()
		if err := <-runErr; err != nil {
			t.Errorf("event loop: %v", err)
		}
	})

	// Queue the storm the way live events do: full renders handed to the
	// tview event queue while keystrokes arrive, so echo latency is
	// measured against real render pressure, not an idle loop.
	const renders = 10
	stormDone := make(chan struct{})
	go func() {
		defer close(stormDone)
		for i := 0; i < renders; i++ {
			select {
			case <-quit:
				return
			default:
			}
			ui.App.QueueUpdateDraw(func() {
				ui.tr.RequestFull()
				ui.refreshMessages()
			})
		}
	}()

	const keys = 5
	injected := make(chan time.Time, keys)
	go func() {
		for i := 0; i < keys; i++ {
			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
			}
			injected <- time.Now()
			sim.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
		}
	}()

	budget := time.Duration(renders)*oneRender + renderGateBudget(t, 500*time.Millisecond)
	var worst time.Duration
	for i := 0; i < keys; i++ {
		at := <-injected
		select {
		case <-echoed:
			if d := time.Since(at); d > worst {
				worst = d
			}
		case <-time.After(budget):
			t.Fatalf("keystroke %d not echoed within budget %s during a render storm (one render %s, worst echo %s)",
				i+1, budget, oneRender, worst)
		}
	}
	select {
	case <-stormDone:
	case <-time.After(30 * time.Second):
		t.Fatal("render storm did not drain")
	}
	t.Logf("echoed %d keystrokes while %d full renders drained (one render %s); worst echo latency %s, budget %s",
		keys, renders, oneRender.Round(time.Millisecond), worst.Round(time.Millisecond), budget.Round(time.Millisecond))
}

// BenchmarkRefreshMessages_SessionScale measures the per-event cost at
// session scale: a forced full render plus draw (the ToolEnd/toggle path)
// against a 175-turn transcript.
func BenchmarkRefreshMessages_SessionScale(b *testing.B) {
	if testing.Short() {
		b.Skip("full-scale benchmark: skipped in -short")
	}
	ui := gateApp()
	sim := setupSim(b, ui)
	growSession(b, ui, 175)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ui.tr.RequestFull()
		ui.refreshMessages()
		ui.Root.Draw(sim)
	}
}
