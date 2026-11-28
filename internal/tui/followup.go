package tui

// submitInput sends the current input text as a new prompt, if any.
//
// Text starting with ":" is dispatched as a colon command instead, so ":steer
// keep it short" reaches the stepper rather than the model.
func (t *App) submitInput() {
	text := t.Input.GetText()
	if text == "" {
		return
	}
	if t.handleColonInput(text) {
		return
	}
	if t.Hooks.OnSubmit != nil {
		t.flushRefresh()
		t.Input.SetText("", false)
		t.Hooks.OnSubmit(text)
	}
}

// submitFollowUp sends the current input text as a follow-up, if any. Colon
// commands are honoured here too: ":" must not surprise the user just because a
// turn is running.
func (t *App) submitFollowUp() {
	text := t.Input.GetText()
	if text == "" {
		return
	}
	if t.handleColonInput(text) {
		return
	}
	if t.Hooks.OnFollowUp != nil {
		t.flushRefresh()
		t.Input.SetText("", false)
		t.Hooks.OnFollowUp(text)
	}
}

// clearConversation resets all conversation state and re-renders the view.
func (t *App) clearConversation() {
	t.tr.Clear()
	// The ledger names blocks that just vanished: an entry (an end waiting for
	// a late start, a dropped start, a lost end) would otherwise settle or
	// strand a block in the next conversation.
	t.ledger.forget()
	t.subAgents = nil
	t.subAgentsBusy.Store(false)
	t.compactBoundary = 0
	t.renderBackgroundJobsPane()
	t.refreshMessages()
}

// doClear clears the conversation and invokes the OnClear callback, if set.
func (t *App) doClear() {
	if t.Hooks.OnClear != nil {
		t.Hooks.OnClear()
	}
	t.clearConversation()
}
