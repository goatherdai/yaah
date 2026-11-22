package tui

import "time"

// Run starts the tview event loop.
func (t *App) Run() error {
	done := t.startBackgroundLoops()
	t.startUIEventLoop(done)
	t.startControlLoop(done)
	t.startActivityTicker(done)
	t.App.SetFocus(t.Input)
	t.renderInfoPane()
	t.renderTodoPane()
	// Seed a launcher prompt (yaah tui workflow run <spec>, piece C) through
	// the same OnSubmit path a typed prompt uses, once the loops above are
	// consuming. queueUpdateDraw routes onto the UI event loop, which blocks
	// on tview until App.Run starts draining it, so this is safe before Run.
	if t.initialPrompt != "" {
		prompt := t.initialPrompt
		t.initialPrompt = ""
		t.queueUpdateDraw(func() {
			if t.OnSubmit != nil {
				t.OnSubmit(prompt)
			}
		})
	}
	err := t.App.Run()
	t.stopBackgroundLoops()
	return err
}

// Stop gracefully shuts down the TUI.
func (t *App) Stop() {
	t.stopBackgroundLoops()
	t.App.Stop()
}

func (t *App) startBackgroundLoops() <-chan struct{} {
	t.bgMu.Lock()
	defer t.bgMu.Unlock()
	if t.bgDone != nil {
		close(t.bgDone)
	}
	t.bgDone = make(chan struct{})
	t.uiEventCh = make(chan uiEvent, uiEventQueueSize)
	return t.bgDone
}

func (t *App) stopBackgroundLoops() {
	t.bgMu.Lock()
	defer t.bgMu.Unlock()
	if t.bgDone == nil {
		return
	}
	close(t.bgDone)
	t.bgDone = nil
	t.uiEventCh = nil
}

func (t *App) startControlLoop(done <-chan struct{}) {
	go func() {
		for {
			select {
			case <-done:
				return
			case msg, ok := <-t.ControlCh:
				if !ok {
					return
				}
				t.App.QueueUpdateDraw(func() {
					t.handleControlMsg(msg)
				})
			}
		}
	}()
}

// startActivityTicker drives the spinner/gauge animation at ~10 Hz, and
// advances the dedicated Subagents pane's per-agent spinners while any
// activity is in flight. The tick is dropped (non-critical queue path)
// when the UI event queue is saturated — worst case the animation
// stutters, it never blocks or crashes.
func (t *App) startActivityTicker(done <-chan struct{}) {
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if !t.activityBusy.Load() && !t.subAgentsBusy.Load() {
					continue
				}
				t.queueUpdateDraw(func() {
					if t.activityBusy.Load() {
						t.activityLine.Pulse()
					}
					t.advanceSubAgentSpinners()
				})
			}
		}
	}()
}
