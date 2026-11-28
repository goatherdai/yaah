package tui

import (
	"time"

	"github.com/goatherdai/yaah/internal/tui/components/question"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// stop.go — the stop interaction: Esc arming, the stop echo, the
// interrupted seal marker, and the Ctrl-C confirm dialog. The stop's
// reach (run in flight, prompt queued on the run gate, wake-started
// turns) lives in the driver's stop hooks — this file only shapes the
// interaction.

// stopArmWindow is how long the first Esc stays armed. Within the window
// a second Esc fires the stop; after it, a lone Esc has gone stale and
// the next one re-arms. Long enough for a deliberate double-press, short
// enough that a stale arm never swallows a later Esc. A var so tests can
// shrink it.
var stopArmWindow = 2 * time.Second

// stopEchoDisplay is the stop echo rendered where a submitted prompt
// would appear, so the halt is visible the moment it is issued — not
// when the run finishes unwinding. Styled like user input.
const stopEchoDisplay = "⏹ interrupted"

// armStop arms the interrupt: the next Esc within stopArmWindow fires.
func (t *App) armStop() {
	t.stopArmedAt = time.Now()
	t.SetEphemeral("esc again to interrupt")
}

// stopArmed reports whether an Esc was pressed within the arming window.
func (t *App) stopArmed() bool {
	return !t.stopArmedAt.IsZero() && time.Since(t.stopArmedAt) < stopArmWindow
}

// requestStop stops the running turn: the echo lands in the transcript
// instantly (optimistically — the seal marker follows at DoneEvent),
// then the driver's stop hook fires. The OnStop/OnAbort fallback order
// mirrors the hook contract: OnAbort only runs when OnStop is unwired.
func (t *App) requestStop() {
	t.stopArmedAt = time.Time{}
	t.EchoStop()
	if t.Hooks.OnStop != nil {
		t.Hooks.OnStop()
		return
	}
	if t.Hooks.OnAbort != nil {
		t.Hooks.OnAbort()
	}
}

// EchoStop flushes in-flight streamed tokens into the conversation and
// renders the stop echo, exactly as EchoPrompt does for a steer: the
// streamed partial seals ahead of the echo instead of after it. When the
// flush painted the partial, stopEchoFlushed latches so the turn's
// DoneEvent does not render DoneEvent.Response a second time — the
// response and the streamed buffer are two renderings of one text, and
// the request-time flush already chose the buffer.
func (t *App) EchoStop() {
	if t.flushPendingTokens() {
		t.stopEchoFlushed.Store(true)
	}
	t.tr.Append(transcript.TextItem(
		t.Theme.Tag(t.Theme.User, stopEchoDisplay),
	))
	t.markDirty()
}

// sealInterrupted appends the dim seal line under the halted turn's
// sealed partial — the "it ended" confirmation that follows the "you
// asked" echo, matching the interrupted tool-block vocabulary.
func (t *App) sealInterrupted() {
	t.tr.Append(transcript.TextItem(t.Theme.Tag(t.Theme.Dim, "◌ interrupted")))
	t.markDirty()
}

// confirmQuit is the Ctrl-C dialog. With a turn running, the primary
// action interrupts it; quitting is a deliberate second choice. Idle,
// it only guards against an accidental quit.
func (t *App) confirmQuit() {
	if t.agentActive {
		t.ShowQuestion("Confirm", "A turn is running.", []struct{ Label, Description string }{
			{"Interrupt the turn", "stop the run; the session stays open"},
			{"Quit", "end the session"},
			{"Cancel", "keep going"},
		}, false, func(a question.Answer) {
			for _, sel := range a.Selected {
				if sel == "Interrupt the turn" {
					t.requestStop()
					return
				}
				if sel == "Quit" {
					t.Stop()
					return
				}
			}
		})
		return
	}
	t.ShowQuestion("Confirm", "Quit yaah?", []struct{ Label, Description string }{
		{"Quit", "end the session"},
		{"Cancel", "keep going"},
	}, false, func(a question.Answer) {
		for _, sel := range a.Selected {
			if sel == "Quit" {
				t.Stop()
				return
			}
		}
	})
}
