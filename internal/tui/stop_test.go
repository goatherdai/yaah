package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/goatherdai/yaah/internal/agent"
)

// stop_test.go — the yaah-ny97 stop interaction: Esc arming, the stop
// echo, the interrupted seal, and the Ctrl-C confirm dialog.

// TestEscapeIdleIsNoOp: with no turn running, Esc neither fires the stop
// hooks nor arms — there is nothing to stop and nothing to arm against.
func TestEscapeIdleIsNoOp(t *testing.T) {
	ui := newColonUI(t)
	stopped := false
	ui.Hooks.OnStop = func() { stopped = true }

	esc := tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone)
	for range 2 {
		if ret := ui.globalInputCapture(esc); ret != nil {
			t.Fatal("idle Esc was not consumed")
		}
	}
	if stopped {
		t.Fatal("idle Esc fired the stop hooks")
	}
	if ui.stopArmed() {
		t.Fatal("idle Esc armed a stop")
	}
}

// TestEscapeArmExpires: an arm goes stale after the window, so a lone Esc
// pressed long before a second one cannot fire a surprise stop.
func TestEscapeArmExpires(t *testing.T) {
	ui := newColonUI(t)
	ui.agentActive = true
	stopped := false
	ui.Hooks.OnStop = func() { stopped = true }

	oldWindow := stopArmWindow
	stopArmWindow = 20 * time.Millisecond
	t.Cleanup(func() { stopArmWindow = oldWindow })

	esc := tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone)
	if ret := ui.globalInputCapture(esc); ret != nil {
		t.Fatal("arming Esc not consumed")
	}
	if stopped {
		t.Fatal("the arming Esc fired the stop")
	}

	time.Sleep(2 * stopArmWindow)

	// The arm has expired: this Esc re-arms instead of firing.
	if ret := ui.globalInputCapture(esc); ret != nil {
		t.Fatal("post-expiry Esc not consumed")
	}
	if stopped {
		t.Fatal("a stale arm fired the stop")
	}
	if ret := ui.globalInputCapture(esc); ret != nil {
		t.Fatal("third Esc not consumed")
	}
	if !stopped {
		t.Fatal("the Esc after the re-arm did not stop the turn")
	}
}

// TestRequestStopEchoesAndFires: the stop echo lands in the transcript
// where a submitted prompt would — the instant feedback that the halt
// was issued — and the driver hook fires exactly once.
func TestRequestStopEchoesAndFires(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	stops := 0
	ui.Hooks.OnStop = func() { stops++ }
	ui.queueUpdate(func() { ui.requestStop() })

	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "⏹ interrupted")
	}, "the stop echo never landed in the transcript")

	ui.queueUpdate(func() {})
	// The hook runs synchronously inside requestStop; the echo is the
	// queued evidence that it ran.
	if stops != 1 {
		t.Fatalf("OnStop calls = %d, want 1", stops)
	}
}

// TestCmdStopRefusesWhenIdle: :stop with no running turn reports instead
// of fabricating a stop echo for a turn that does not exist.
func TestCmdStopRefusesWhenIdle(t *testing.T) {
	ui := newColonUI(t)
	stopped := false
	ui.Hooks.OnStop = func() { stopped = true }

	ui.Input.SetText(":stop", true)
	ui.submitInput()

	if stopped {
		t.Fatal(":stop fired with no turn running")
	}
	if got := probeEphemeral(t, ui); !strings.Contains(got, "no turn is running") {
		t.Fatalf("ephemeral = %q, want the idle refusal", got)
	}
}

// TestDoneEventInterruptedSealsWithoutDialog: a deliberate stop's
// DoneEvent seals the partial with the interrupted marker and raises no
// error dialog — the "context canceled" modal was the yaah-ny97 AC.
func TestDoneEventInterruptedSealsWithoutDialog(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.DoneEvent{Response: "the partial answer", Interrupted: true, ContextWindow: 8000})

	waitForUI(t, ui, func() bool {
		painted := ui.tr.PaintedText()
		return strings.Contains(painted, "the partial answer") && strings.Contains(painted, "◌ interrupted")
	}, "the interrupted seal marker never landed under the partial")

	ui.queueUpdate(func() {
		if ui.Pages.HasPage("error_dialog") {
			t.Error("an interrupted run opened the error dialog")
		}
		if ui.stopArmed() {
			t.Error("a stale arm survived the turn's DoneEvent")
		}
	})
}

// TestDoneEventErrorStillOpensDialog: the filter is the interrupted
// classification, not a blanket silence — a real run error still surfaces.
func TestDoneEventErrorStillOpensDialog(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.DoneEvent{Response: "", Error: "provider exploded", ContextWindow: 8000})

	waitForUI(t, ui, func() bool {
		return ui.Pages.HasPage("error_dialog")
	}, "the run error never opened the dialog")
}

// TestCtrlCConfirmsInsteadOfQuitting: Ctrl-C at normal focus opens the
// confirm dialog rather than ending the session outright, with the
// interrupt choice offered only while a turn runs.
func TestCtrlCConfirmsInsteadOfQuitting(t *testing.T) {
	for _, active := range []bool{false, true} {
		ui := newColonUI(t)
		ui.agentActive = active

		ctrlC := tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone)
		if ret := ui.globalInputCapture(ctrlC); ret != nil {
			t.Fatalf("Ctrl-C not consumed (active=%v)", active)
		}
		if !ui.Pages.HasPage("question_modal") {
			t.Fatalf("Ctrl-C did not open the confirm dialog (active=%v)", active)
		}
		if ui.focus != focusModal {
			t.Fatalf("the confirm dialog did not claim modal focus (active=%v)", active)
		}
	}
}

// TestCtrlCInModalKeepsEscapeHatch: with a layered view up, Ctrl-C is
// passed through to tview's built-in quit instead of stacking another
// dialog — the escape hatch stays armed behind every overlay.
func TestCtrlCInModalKeepsEscapeHatch(t *testing.T) {
	ui := newColonUI(t)
	ui.focus = focusModal

	ctrlC := tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone)
	if ret := ui.globalInputCapture(ctrlC); ret == nil {
		t.Fatal("Ctrl-C in a modal was consumed instead of passed through")
	}
	if ui.Pages.HasPage("question_modal") {
		t.Fatal("Ctrl-C in a modal stacked a confirm dialog")
	}
}

// TestStopEchoDoesNotDuplicateSealedPartial pins the render invariant
// behind the stop echo: the streamed buffer and DoneEvent.Response are
// two renderings of one text, so after the echo's flush painted the
// partial, the DoneEvent must not paint it a second time.
func TestStopEchoDoesNotDuplicateSealedPartial(t *testing.T) {
	ui := gateApp()
	done := make(chan struct{})
	defer close(done)
	drainUIEvents(ui, done)

	ui.HandleEvent(&agent.TokenDeltaEvent{Text: "the partial"})
	ui.queueUpdate(func() { ui.requestStop() })
	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "⏹ interrupted")
	}, "the stop echo never landed")

	ui.HandleEvent(&agent.DoneEvent{Response: "the partial", Interrupted: true, ContextWindow: 8000})
	waitForUI(t, ui, func() bool {
		return strings.Contains(ui.tr.PaintedText(), "◌ interrupted")
	}, "the seal marker never landed")

	ui.queueUpdate(func() {
		if got := strings.Count(ui.tr.PaintedText(), "the partial"); got != 1 {
			t.Errorf("the partial rendered %d times, want exactly 1", got)
		}
	})
}
