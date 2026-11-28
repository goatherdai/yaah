package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/toolfmt"
	"github.com/goatherdai/yaah/internal/tui/components/activity"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

// toolBlockKey picks the id a tool block is keyed on. The event's session-unique
// Key is authoritative; a keyless event (a legacy or test dispatch) falls back to
// the run-scoped ordinal, matching the pre-yaah-77f1 behaviour rather than
// opening an empty-key block. The other views carry the same fallback
// (acp.correlateID, the web wire's tool_id), so the TUI is not the lone exception.
//
// That the fallback repeats on every prompt is handled where it matters — in the
// ledger, which matches records on (turn generation, key) rather than the key
// alone, so a keyless call can never settle another turn's block (yaah-ol3r).
func toolBlockKey(key string, id int64) string {
	if key != "" {
		return key
	}
	return strconv.FormatInt(id, 10)
}

func (t *App) HandleEvent(event agent.Event) {
	switch e := event.(type) {
	case *agent.TokenDeltaEvent:
		t.tokensRx.Add(1)
		t.tokenMu.Lock()
		t.pendingTokens.WriteString(e.Text)
		first := !t.isStreaming.Swap(true)
		t.tokenMu.Unlock()
		if first {
			t.queueUpdateDrawCritical(func() {
				t.setActivity(activity.Responding, "")
			})
		}
	case *agent.ThinkingEvent:
		t.queueThinkingUpdate(e.Text)
	case *agent.FlushEvent:
		t.queueUpdate(func() {
			t.flushPendingTokens()
		})
	case *agent.ToolStartEvent:
		// The block is keyed on the event's session-unique identity.
		// e.ID is the run's dispatch ordinal (it restarts at 1 on every
		// prompt), so keying on it alone made a later prompt's call
		// settle an earlier prompt's block (yaah-77f1). A dropped update
		// is counted below: the block it would have created never exists.
		key := toolBlockKey(e.Key, e.ID)
		// The generation is read HERE, on the forwarder goroutine in event
		// order: reading it when the queued update runs would let a later
		// turn's DoneEvent bump it first, which is exactly the cross-turn
		// confusion the ledger's generation exists to prevent.
		gen := t.turnGen.Load()
		scheduled := t.queueUpdateCritical(func() {
			t.pendingTool = e.Name
			if e.Name == "delegate" {
				return
			}
			t.setActivity(activity.Tool, e.Name)
			t.addToolStart(gen, key, e.Name, e.Args)
		})
		if !scheduled && e.Name != "delegate" {
			// The update will never run, so no block exists for this
			// call. Count the loss here, where it is observed, and record
			// it so the turn-end sweep does not also attribute the block
			// — and does not keep waiting for a start that cannot come.
			// A delegate draws no tool block, so its loss is not a block
			// loss.
			t.ledger.markStartDropped(gen, key)
			t.recordToolOrphan(orphanKindDroppedStart)
		}
	case *agent.ToolEndEvent:
		// The same key the start used: a dropped end is recorded under it so
		// the turn-end sweep can attribute the block it could not settle.
		key := toolBlockKey(e.Key, e.ID)
		gen := t.turnGen.Load()
		scheduled := t.queueUpdateCritical(func() {
			t.pendingTool = ""
			if e.Name == "delegate" {
				return
			}
			if e.Error != "" {
				t.settleToolEnd(gen, key, transcript.ToolEnd{
					Summary:  e.Name + " \u274C",
					Result:   e.Error,
					Duration: e.Duration,
					Error:    true,
				})
			} else {
				t.settleToolEnd(gen, key, transcript.ToolEnd{
					Summary:  toolfmt.Summary(e.Name, e.Args, e.Result),
					Result:   e.Result,
					Duration: e.Duration,
				})
			}
			t.restoreActivity()
		})
		if !scheduled && e.Name != "delegate" {
			// The end will never be applied. Count it as the drop it is
			// and record it, so the turn-end sweep — or, if the start is
			// still in flight, the start itself — seals the block (never
			// a permanent chevron) without claiming the loss as its own.
			t.ledger.markEndLost(gen, key)
			t.recordToolOrphan(orphanKindDroppedEnd)
		}
	case *agent.SubAgentStartEvent:
		t.queueUpdateDrawCritical(func() {
			t.AddSubAgentStart(e.SubAgentID, e.Role, e.Prompt,
				toolfmt.SubagentWorldLabel(e.World, e.WorldDegraded, e.WorldReason))
			t.setActivity(activity.SubAgent, activity.FormatSubAgentDetail(e.Role, len(t.subAgents)))
		})
	case *agent.SubAgentEndEvent:
		t.queueUpdateDrawCritical(func() {
			if e.Error != "" {
				t.AddSubAgentError(e.SubAgentID)
			} else {
				t.AddSubAgentEnd(e.SubAgentID)
			}
			if len(t.subAgents) > 0 {
				t.activityLine.SetState(activity.SubAgent, activity.FormatSubAgentDetail(e.Role, len(t.subAgents)))
			} else {
				t.restoreActivity()
			}
		})
	case *agent.EscalationEvent:
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			msg := fmt.Sprintf("[%s]\u26A0 %s[-]", t.Theme.Error, e.Summary)
			t.tr.Append(transcript.TextItem(msg))
			t.markDirty()
		})
	case *agent.CompactionStartedEvent:
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			t.compacting = true
			// Record the collapse boundary: if compaction completes, the
			// view reconciles (yaah-30t6 AC 3) by replacing everything
			// below this mark with a one-line stub.
			t.compactBoundary = t.tr.Len()
			detail := fmt.Sprintf("%.1fK\u2192%.1fK", float64(e.BeforeTokens)/1000, float64(e.TargetTokens)/1000)
			t.setActivity(activity.Compacting, detail)
			msg := fmt.Sprintf("[%s]%s[-]", t.Theme.Dim, e.StartedSummary())
			t.tr.Append(transcript.TextItem(msg))
			t.markDirty()
		})
	case *agent.CompactionDoneEvent:
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			t.compacting = false
			t.restoreActivity()
			// The compacted-away messages no longer exist upstream;
			// collapse them to a stub so the painted transcript
			// matches the agent's context instead of keeping the full
			// pre-compaction scrollback. The buffer head changes, so the
			// next refresh is a full render (and re-windows).
			t.collapseCompacted()
			t.compactBoundary = 0
			msg := fmt.Sprintf("[%s]%s[-]", t.Theme.Dim, e.DoneSummary())
			t.tr.Append(transcript.TextItem(msg))
			t.markDirty()
		})
	case *agent.TitleSetEvent:
		// Intentionally ignored: the TUI's in-app session display does
		// not track titles yet; pickers read them from the store.
	case *agent.AgentChangedEvent:
		// In-band persona marker in the conversation, next to the compaction
		// notes — session start and mid-session switches (including the
		// deprecated mode aliases): which persona the session runs as stays
		// in the transcript, not only in the info pane. Only non-build
		// personas announce at start (yaah-21uk.10).
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			msg := fmt.Sprintf("[%s]%s[-]", t.Theme.Dim, e.Notice())
			t.tr.Append(transcript.TextItem(msg))
			t.markDirty()
		})
	case *agent.ModeChangedEvent:
		// In-band mode marker, same transcript line as a persona switch:
		// the regime the session now runs stays visible in the
		// conversation, next to the persona switch notices. dispatchEvent
		// buffers it for views that cannot take writes between runs.
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			msg := fmt.Sprintf("[%s]%s[-]", t.Theme.Dim, e.Notice())
			t.tr.Append(transcript.TextItem(msg))
			t.markDirty()
		})
	case *agent.WakeEvent:
		// A watch fired between turns: record the injected prompt exactly
		// where a user turn would appear, so the transcript shows what woke
		// the session and which evidence came with it.
		t.queueUpdateCritical(func() {
			t.flushPendingTokens()
			t.addWakePrompt(e.Text)
		})
	case *agent.DoneEvent:
		// The generation is closed HERE, in event order: every tool event of
		// this turn was already stamped with gen, and any event of the next
		// turn will be stamped gen+1 — even if it is handled before this
		// sweep's closure runs on the UI goroutine. That is what makes a
		// turn's records impossible to consume from another turn.
		gen := t.turnGen.Load()
		t.turnGen.Add(1)
		scheduled := t.queueUpdateDrawCritical(func() {
			flushed := t.flushPendingTokens()
			t.pendingTool = ""
			// A stale arm must not carry into the next turn: one Esc
			// then must arm again, not fire.
			t.stopArmedAt = time.Time{}
			echoFlushed := t.stopEchoFlushed.Swap(false)
			// Self-healing seal: any tool block still Running has no end
			// event coming — the turn is over. Sweep it to a terminal
			// state with an interrupted marker instead of leaving a
			// permanent chevron (yaah-ebhe), and attribute this turn's
			// losses once.
			t.sweepTurn(gen)
			t.setActivity(activity.Idle, "")
			// The response renders only when neither the stop echo's
			// flush nor this flush painted it: the streamed buffer and
			// DoneEvent.Response are two renderings of one text, and
			// the stop path already flushed the buffer at request time.
			if !flushed && !echoFlushed && e.Response != "" {
				t.addAssistantResponse(e.Response)
			}
			// A deliberate stop seals with the interrupted marker
			// (yaah-ny97): the partial is already in the transcript —
			// streamed, or just appended above — so the run's end is
			// recorded, not raised as a "context canceled" error.
			if e.Interrupted {
				t.sealInterrupted()
			}
			if e.Error != "" {
				t.showErrorDialog("Error", e.Error)
			}
			t.markDirty()

			if e.ContextWindow > 0 {
				ct := e.ContextTokens
				if e.LastPromptTokensInclusive > 0 {
					ct = e.LastPromptTokensInclusive
				} else if e.LastPromptTokens > 0 {
					ct = e.LastPromptTokens
				}
				t.contextTokens = ct
				t.contextWindow = e.ContextWindow
			}
			t.renderInfoPane()
		})
		if !scheduled {
			// The turn-end reconciliation never runs: nothing sweeps this
			// turn, so a still-Running block keeps its chevron. The sweep
			// cannot be run here — the transcript belongs to the UI
			// goroutine — and re-queueing it could seal the NEXT turn's
			// blocks, so the loss is recorded instead of recovered.
			//
			// No record needs discarding here: the entries this sweep would
			// have retired are stamped gen, the next turn's events are stamped
			// gen+1, and attribution matches on the generation — so a stale
			// entry cannot suppress the next turn's accounting, and the next
			// sweep retires it as stale (yaah-ol3r).
			t.recordToolOrphan(orphanKindDroppedSweep)
		}
	}
}

// collapseCompacted replaces the pre-compaction conversation prefix with a
// one-line stub (yaah-30t6 AC 3): compaction removed those messages from
// the agent's context, so the view stops painting them too.
func (t *App) collapseCompacted() {
	t.tr.CollapseCompacted(t.compactBoundary)
}

func (t *App) flushPendingTokens() bool {
	if !t.isStreaming.Load() {
		return false
	}
	t.tokenMu.Lock()
	if t.pendingTokens.Len() == 0 {
		t.tokenMu.Unlock()
		return false
	}
	raw := t.pendingTokens.String()
	t.pendingTokens.Reset()
	t.tokenMu.Unlock()

	_, span := otel.Tracer("yaah").Start(context.Background(), "tui.flush",
		trace.WithAttributes(
			attribute.Int64("tokens_rx", t.tokensRx.Load()),
			attribute.Int64("chars_written", t.charsWritten.Load()),
			attribute.Int64("chars_rendered", t.tr.CharsRendered()),
			attribute.Int("pending_len", len(raw)),
		))
	span.End()

	t.addAssistantResponse(raw)
	t.isStreaming.Store(false)
	return true
}
