package tui

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/toolfmt"
	"github.com/goatherdai/yaah/internal/tui/components/activity"
	"github.com/goatherdai/yaah/internal/tui/transcript"
)

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
		t.queueUpdateCritical(func() {
			t.pendingTool = e.Name
			if e.Name == "delegate" {
				return
			}
			t.setActivity(activity.Tool, e.Name)
			id := fmt.Sprintf("%d", e.ID)
			t.AddToolStart(id, e.Name, e.Args)
		})
	case *agent.ToolEndEvent:
		t.queueUpdateCritical(func() {
			t.pendingTool = ""
			if e.Name == "delegate" {
				return
			}
			id := fmt.Sprintf("%d", e.ID)
			if e.Error != "" {
				t.AddToolError(id, e.Name+" \u274C", e.Error)
			} else {
				t.AddToolEnd(id, toolfmt.Summary(e.Name, e.Args, e.Result), e.Result)
			}
			t.restoreActivity()
		})
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
		t.queueUpdateDrawCritical(func() {
			flushed := t.flushPendingTokens()
			t.pendingTool = ""
			// A stale arm must not carry into the next turn: one Esc
			// then must arm again, not fire.
			t.stopArmedAt = time.Time{}
			// Self-healing seal: any tool block still Running has no end
			// event coming — the turn is over. Sweep it to a terminal
			// state with an interrupted marker instead of leaving a
			// permanent chevron (yaah-ebhe).
			t.SweepStuckToolBlocks()
			t.setActivity(activity.Idle, "")
			if !flushed && e.Response != "" {
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
