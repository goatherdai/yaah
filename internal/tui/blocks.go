package tui

import (
	"context"
	"time"

	"github.com/goatherdai/yaah/internal/observability"
	"github.com/goatherdai/yaah/internal/tui/components/reasoning"
	"github.com/goatherdai/yaah/internal/tui/components/subagent"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// blocks.go — block operations (reasoning, tool, sub-agent). Reasoning
// and tool blocks are transcript items; their in-place mutations go
// through the model's mutators, which bump the item's generation and
// mark the model for a full repaint. Sub-agents are not transcript
// items; they render in their own pane.

// AddReasoningBlock creates a reasoning block and appends it to the
// transcript.
func (t *App) AddReasoningBlock(id, content string) {
	rb := reasoning.New(id, content, 0, t.Theme)
	t.tr.Append(transcript.ReasoningItem(rb))
	t.markDirty()
}

// Orphan kinds recorded on the yaah.tui.tool_orphans counter.
const (
	// orphanKindUnmatchedEnd: an end/error event arrived whose tool block
	// does not exist — dropped, reordered, or belonging to a cleared log.
	orphanKindUnmatchedEnd = "unmatched_end"
	// orphanKindSwept: a still-Running block sealed by the turn-end sweep.
	// After the drop accounting below, this means only that the call's end
	// never arrived AND no seam observed it being lost.
	orphanKindSwept = "swept"
	// orphanKindLateEnd: an end event healed a block the turn-end sweep had
	// already sealed as Interrupted — the event was merely late.
	orphanKindLateEnd = "late_end"
	// orphanKindDuplicateEnd: an end event met a block that was already
	// terminal. Nothing was overwritten.
	orphanKindDuplicateEnd = "duplicate_end"
	// orphanKindDroppedStart: a tool start event's UI update could not be
	// scheduled, so no block was ever created for the call.
	orphanKindDroppedStart = "dropped_start"
	// orphanKindDroppedEnd: a tool end event's UI update could not be
	// scheduled. The block is still sealed by the turn-end sweep (a block
	// must never render a permanent chevron), but the loss is attributed
	// here, where it was observed, not to the sweep.
	orphanKindDroppedEnd = "dropped_end"
	// orphanKindDroppedSweep: the DoneEvent's turn-end reconciliation could
	// not be scheduled. Nothing sweeps this turn, so any still-Running block
	// keeps its chevron and the lost-end record is never consumed. The
	// sweep itself cannot be run from the event goroutine — the transcript
	// is owned by the UI goroutine — so the loss is recorded instead of
	// recovered: it is the only seam that observes it.
	orphanKindDroppedSweep = "dropped_sweep"
)

// lostToolEndsLimit bounds the record of end events whose UI update could never
// be scheduled. The counter is the durable signal; a few keys are kept so the
// turn-end sweep can attribute the blocks they belong to.
const lostToolEndsLimit = 16

// recordToolOrphan records one tool-block orphan kind: on the durable OTel
// counter, and on an in-process tally. The tally exists because the instruments
// are bound only in a metrics-configured process, so the classification would
// otherwise be unwitnessable — the same reason the queue outcomes keep
// uiEventDrops beside RecordTUIQueueEvent.
func (t *App) recordToolOrphan(kind string) {
	t.toolOrphanMu.Lock()
	if t.toolOrphans == nil {
		t.toolOrphans = make(map[string]int, 6)
	}
	t.toolOrphans[kind]++
	t.toolOrphanMu.Unlock()
	observability.RecordTUIToolOrphan(context.Background(), kind)
}

// toolOrphanCounts returns a copy of the orphan tally, keyed by kind.
func (t *App) toolOrphanCounts() map[string]int {
	t.toolOrphanMu.Lock()
	defer t.toolOrphanMu.Unlock()
	out := make(map[string]int, len(t.toolOrphans))
	for kind, n := range t.toolOrphans {
		out[kind] = n
	}
	return out
}

// recordLostToolEnd remembers that key's end event will never be applied, so
// the turn-end sweep can attribute the block to the drop instead of to its own
// recovery. It runs on the agent-loop goroutine (HandleEvent's), so the record
// is mutex-guarded: the sweep reads it on the UI goroutine.
func (t *App) recordLostToolEnd(key string) {
	t.lostEndsMu.Lock()
	defer t.lostEndsMu.Unlock()
	for _, k := range t.lostEnds {
		if k == key {
			return
		}
	}
	if len(t.lostEnds) >= lostToolEndsLimit {
		copy(t.lostEnds, t.lostEnds[1:])
		t.lostEnds = t.lostEnds[:len(t.lostEnds)-1]
	}
	t.lostEnds = append(t.lostEnds, key)
}

// takeLostToolEnds returns and clears the recorded lost ends. The sweep is the
// turn boundary: a loss recorded before it belongs to a block the sweep is
// about to seal, and a later sweep must not re-attribute it.
func (t *App) takeLostToolEnds() map[string]bool {
	t.lostEndsMu.Lock()
	defer t.lostEndsMu.Unlock()
	if len(t.lostEnds) == 0 {
		return nil
	}
	lost := make(map[string]bool, len(t.lostEnds))
	for _, k := range t.lostEnds {
		lost[k] = true
	}
	t.lostEnds = nil
	return lost
}

// AddToolError settles the block carrying key into its error state, and
// counts how the end met the transcript (see transcript.SettleTool).
func (t *App) AddToolError(key, summary, err string, dur time.Duration) {
	t.settleToolBlock(key, transcript.ToolEnd{Summary: summary, Result: err, Duration: dur, Error: true})
}

// AddToolStart adds a tool block to the transcript.
// Note: flushes any pending streaming tokens first.
//
// A start can be applied AFTER its own end: the critical-update fallback runs an
// end directly when the UI queue is saturated while the start still waits in
// the queue (internal/tui/event_queue.go). The settlement rule stashes such an
// end (transcript.SettleTool), so the block is created from it already settled
// rather than Running — a Running block would wait for an end that is already
// behind it.
func (t *App) AddToolStart(key, name, args string) {
	t.flushPendingTokens()
	tb := toolblock.New(key, name, args, t.Theme)
	if end, ok := t.tr.TakeUnmatchedEnd(key); ok {
		state := toolblock.Done
		if end.Error {
			state = toolblock.Error
		}
		tb.Seal(state, end.Summary, end.Result, end.Duration)
	}
	t.tr.Append(transcript.ToolItem(tb))
	t.markDirty()
}

// AddToolEnd settles the block carrying key with the call's result.
// The caller (proxy.go) is responsible for routing errors via AddToolError.
// Tools may legitimately return empty results (e.g., successful delete with no output).
func (t *App) AddToolEnd(key, summary, result string, dur time.Duration) {
	t.settleToolBlock(key, transcript.ToolEnd{Summary: summary, Result: result, Duration: dur})
}

// settleToolBlock applies one end event through the transcript's single
// settlement rule and surfaces whatever it could not do: a healed or
// duplicated end is real drift in the event stream and is counted, while a
// settled block only needs its repaint scheduled.
func (t *App) settleToolBlock(key string, end transcript.ToolEnd) {
	switch t.tr.SettleTool(key, end) {
	case transcript.Settled:
		t.markDirty()
	case transcript.LateEnd:
		t.markDirty()
		t.recordToolOrphan(orphanKindLateEnd)
	case transcript.DuplicateEnd:
		t.recordToolOrphan(orphanKindDuplicateEnd)
	case transcript.UnmatchedEnd:
		t.recordToolOrphan(orphanKindUnmatchedEnd)
	}
}

// SweepStuckToolBlocks seals every still-Running tool block at turn end: its
// end event never arrived, and without the sweep it would render a permanent
// chevron (the stuck-tool-block finding). Each sealed block renders terminal
// with an interrupted marker.
//
// The sweep is the safety net, not the attribution of first resort: a block
// whose end event was observed to drop carries that attribution already
// (recordLostToolEnd at the drop), so it is sealed here without also being
// counted as swept.
func (t *App) SweepStuckToolBlocks() int {
	lost := t.takeLostToolEnds()
	sealed := t.tr.SweepRunningTools()
	if len(sealed) == 0 {
		return 0
	}
	t.markDirty()
	for _, key := range sealed {
		if lost[key] {
			continue
		}
		t.recordToolOrphan(orphanKindSwept)
	}
	return len(sealed)
}

// AddSubAgentStart adds a sub-agent to the dedicated Subagents pane.
// Sub-agents are not part of the transcript; the pane is their only
// display surface.
func (t *App) AddSubAgentStart(id, role, task, world string) {
	sb := subagent.New(id, role, task)
	sb.SetWorld(world)
	t.subAgents = append(t.subAgents, sb)
	t.subAgentsBusy.Store(true)
	t.renderBackgroundJobsPane()
}

// AddSubAgentEnd completes a sub-agent and drops it from the pane. The pane
// shows only running work, so keeping terminal blocks would be dead state.
func (t *App) AddSubAgentEnd(id string) {
	if sb := t.findSubAgent(id); sb != nil {
		sb.Complete()
		t.removeSubAgent(id)
		t.syncSubAgentBusy()
		t.renderBackgroundJobsPane()
	}
}

// AddSubAgentError fails a sub-agent and drops it from the pane.
func (t *App) AddSubAgentError(id string) {
	if sb := t.findSubAgent(id); sb != nil {
		sb.Fail()
		t.removeSubAgent(id)
		t.syncSubAgentBusy()
		t.renderBackgroundJobsPane()
	}
}

// findSubAgent returns the sub-agent block with the given id, if any.
func (t *App) findSubAgent(id string) *subagent.Block {
	for _, sb := range t.subAgents {
		if sb.ID() == id {
			return sb
		}
	}
	return nil
}

// removeSubAgent drops a terminal block from the pane's active set.
func (t *App) removeSubAgent(id string) {
	for i, sb := range t.subAgents {
		if sb.ID() == id {
			t.subAgents = append(t.subAgents[:i], t.subAgents[i+1:]...)
			return
		}
	}
}

// syncSubAgentBusy updates the atomic flag that keeps the animation ticker
// alive while sub-agents run, including background dispatches that outlive
// the turn's activity-line state.
func (t *App) syncSubAgentBusy() {
	for _, sb := range t.subAgents {
		if sb.S() == subagent.Active {
			t.subAgentsBusy.Store(true)
			return
		}
	}
	t.subAgentsBusy.Store(false)
}

// advanceSubAgentSpinners advances every active sub-agent's spinner and
// repaints the pane. Driven by the activity ticker while agents are running.
func (t *App) advanceSubAgentSpinners() {
	advanced := false
	for _, sb := range t.subAgents {
		if sb.S() == subagent.Active {
			sb.AdvanceSpinner()
			advanced = true
		}
	}
	if advanced {
		t.renderBackgroundJobsPane()
	}
}

// ToggleBlockByIndex toggles the nth toggleable block (reasoning or tool).
func (t *App) ToggleBlockByIndex(n int) {
	if t.tr.ToggleBlockByIndex(n) {
		t.refreshMessages()
	}
}

// CollapseAll collapses all expandable blocks.
func (t *App) CollapseAll() {
	t.tr.CollapseAll()
	t.refreshMessages()
}

func (t *App) toggleAllReasoning() {
	t.tr.ToggleAllReasoning()
	t.refreshMessages()
}

func (t *App) toggleAllTools() {
	t.tr.ToggleAllTools()
	t.refreshMessages()
}
