package tui

import (
	"context"

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
	orphanKindSwept = "swept"
)

// AddToolError finds a tool block in the transcript by ID and
// transitions it to Error. An unmatched ID is counted as an orphan: the
// event was dropped or reordered, and the counter makes the drift
// visible instead of silently discarding it.
func (t *App) AddToolError(id, summary, err string) {
	if t.tr.FailTool(id, summary, err) {
		t.markDirty()
		return
	}
	observability.RecordTUIToolOrphan(context.Background(), orphanKindUnmatchedEnd)
}

// AddToolStart adds a tool block to the transcript.
// Note: flushes any pending streaming tokens first.
func (t *App) AddToolStart(id, name, args string) {
	t.flushPendingTokens()
	tb := toolblock.New(id, name, args, t.Theme)
	t.tr.Append(transcript.ToolItem(tb))
	t.markDirty()
}

// AddToolEnd updates a tool block in the transcript with its result.
// The caller (proxy.go) is responsible for routing errors via AddToolError.
// Tools may legitimately return empty results (e.g., successful delete with no output).
func (t *App) AddToolEnd(id, summary, result string) {
	if t.tr.CompleteTool(id, summary, result) {
		t.markDirty()
		return
	}
	observability.RecordTUIToolOrphan(context.Background(), orphanKindUnmatchedEnd)
}

// SweepStuckToolBlocks seals every still-Running tool block at turn end:
// its end event never arrived, and without the sweep it would render a
// permanent chevron (the stuck-tool-block finding). Each sealed block
// renders terminal with an interrupted marker.
func (t *App) SweepStuckToolBlocks() int {
	n := t.tr.SweepRunningTools()
	if n > 0 {
		t.markDirty()
		observability.RecordTUIToolOrphan(context.Background(), orphanKindSwept)
	}
	return n
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
