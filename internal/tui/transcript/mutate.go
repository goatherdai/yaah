package transcript

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// mutate.go — in-place item mutations, log rewrites, and search. Every
// mutation that can change a painted item's row count bumps the item's
// gen (invalidating its caches) and marks the model for a full repaint.

// ToolEnd is the part of a tool's end event the transcript needs to settle a
// block: what the block should show, and how long the call itself took.
type ToolEnd struct {
	Summary  string
	Result   string
	Duration time.Duration
	// Error selects the block's terminal state: Error rather than Done.
	Error bool
}

// Settlement classifies how one tool end event met the transcript's blocks.
type Settlement int

const (
	// Settled: the end settled a Running block.
	Settled Settlement = iota
	// LateEnd: the end healed a block the turn-end sweep had already sealed
	// as Interrupted.
	LateEnd
	// DuplicateEnd: the block was already terminal, so nothing was written.
	DuplicateEnd
	// UnmatchedEnd: no block carried the key; the end was stashed.
	UnmatchedEnd
)

// unmatchedEndsLimit bounds the unmatched-end stash. Settlement failures are
// evidence about ordering, so a few are kept for diagnosis; the counter at the
// view layer is the durable signal, not this ring.
const unmatchedEndsLimit = 8

// UnmatchedToolEnd is an end event that settled no block, kept so the block its
// key names can be created already settled if the start is merely late, and so
// a caller can inspect the drift instead of only counting it.
type UnmatchedToolEnd struct {
	Key string
	End ToolEnd
}

// SettleTool applies one tool end event to the block its key names. There is
// one rule for every end, success or failure, in order:
//
//  1. the LAST Running block with the key settles — a later run's call is the
//     live one, so an earlier block that happens to carry the same key is
//     never settled by it;
//  2. else an Interrupted block with the key heals (LateEnd): the turn-end
//     sweep sealed it before its end arrived, and it now shows the real
//     outcome, labelled with the event's own duration rather than the delay;
//  3. else a block with the key is already terminal and is left untouched
//     (DuplicateEnd): a second end never overwrites the first;
//  4. else nothing matched, so the end is stashed (UnmatchedEnd).
func (m *Model) SettleTool(key string, end ToolEnd) Settlement {
	lastRunning, lastInterrupted, matched := -1, -1, false
	for i := len(m.items) - 1; i >= 0; i-- {
		tb := m.items[i].ToolBlock
		if tb == nil || tb.ID() != key {
			continue
		}
		matched = true
		switch {
		case tb.IsRunning() && lastRunning < 0:
			lastRunning = i
		case tb.S() == toolblock.Interrupted && lastInterrupted < 0:
			lastInterrupted = i
		}
	}

	state := toolblock.Done
	if end.Error {
		state = toolblock.Error
	}
	switch {
	case lastRunning >= 0:
		m.sealTool(lastRunning, state, end)
		return Settled
	case lastInterrupted >= 0:
		m.sealTool(lastInterrupted, state, end)
		return LateEnd
	case matched:
		return DuplicateEnd
	default:
		m.stashUnmatchedEnd(key, end)
		return UnmatchedEnd
	}
}

// sealTool writes one settlement outcome into the block at index i and marks
// the painted rows stale.
func (m *Model) sealTool(i int, state toolblock.State, end ToolEnd) {
	m.items[i].ToolBlock.Seal(state, end.Summary, end.Result, end.Duration)
	m.items[i].gen++
	m.dirty = dirtyFull
}

// stashUnmatchedEnd records an end that settled no block, dropping the oldest
// once the ring is full.
func (m *Model) stashUnmatchedEnd(key string, end ToolEnd) {
	if len(m.unmatchedEnds) >= unmatchedEndsLimit {
		copy(m.unmatchedEnds, m.unmatchedEnds[1:])
		m.unmatchedEnds = m.unmatchedEnds[:len(m.unmatchedEnds)-1]
	}
	m.unmatchedEnds = append(m.unmatchedEnds, UnmatchedToolEnd{Key: key, End: end})
}

// UnmatchedEnds returns the end events settled by the UnmatchedEnd rule,
// oldest first. It is diagnostic state, not a rendering input.
func (m *Model) UnmatchedEnds() []UnmatchedToolEnd {
	out := make([]UnmatchedToolEnd, len(m.unmatchedEnds))
	copy(out, m.unmatchedEnds)
	return out
}

// TakeUnmatchedEnd removes and returns the stashed end that settled no block
// for key, reporting whether there was one.
//
// A caller about to create the block for key uses it to create that block
// already settled. An end can be applied BEFORE its own start: the view's
// critical-update fallback runs an end directly when the UI queue is saturated
// while an earlier start is still queued behind the full queue
// (internal/tui/event_queue.go), so the block must inherit the end that
// already arrived — a block created Running in that case would wait for an
// event that is behind it and be sealed by the sweep as interrupted.
func (m *Model) TakeUnmatchedEnd(key string) (ToolEnd, bool) {
	for i, ue := range m.unmatchedEnds {
		if ue.Key != key {
			continue
		}
		m.unmatchedEnds = append(m.unmatchedEnds[:i], m.unmatchedEnds[i+1:]...)
		return ue.End, true
	}
	return ToolEnd{}, false
}

// SweepRunningTools seals every still-Running tool block as
// interrupted. The turn is over when the sweep runs (the agent loop's
// DoneEvent), so a Running block's end event never arrived — without the
// sweep it renders a permanent chevron. It reports the keys it sealed, so a
// caller can attribute each block without re-reading the log.
//
// The sweep also clears the unmatched-end stash: an end stashed in this turn
// whose start never arrived has no block coming, and keeping it would let a
// later turn's start be settled by an earlier turn's event.
func (m *Model) SweepRunningTools() []string {
	sealed := make([]string, 0, 4)
	for i := range m.items {
		if tb := m.items[i].ToolBlock; tb != nil && tb.IsRunning() {
			tb.Interrupt()
			m.items[i].gen++
			sealed = append(sealed, tb.ID())
		}
	}
	m.unmatchedEnds = nil
	if len(sealed) > 0 {
		m.dirty = dirtyFull
	}
	return sealed
}

// ToggleBlockByIndex toggles the nth toggleable block (reasoning or
// tool) and reports whether one existed at that index.
func (m *Model) ToggleBlockByIndex(n int) bool {
	idx := -1
	for i := range m.items {
		it := &m.items[i]
		if it.ToolBlock != nil || it.ReasBlock != nil {
			idx++
			if idx == n {
				if it.ToolBlock != nil {
					it.ToolBlock.Toggle()
				} else {
					it.ReasBlock.Toggle()
				}
				it.gen++
				m.dirty = dirtyFull
				return true
			}
		}
	}
	return false
}

// ToggleAllReasoning toggles every reasoning block.
func (m *Model) ToggleAllReasoning() {
	for i := range m.items {
		if rb := m.items[i].ReasBlock; rb != nil {
			rb.Toggle()
			m.items[i].gen++
		}
	}
	m.dirty = dirtyFull
}

// ToggleAllTools toggles every tool block.
func (m *Model) ToggleAllTools() {
	for i := range m.items {
		if tb := m.items[i].ToolBlock; tb != nil {
			tb.Toggle()
			m.items[i].gen++
		}
	}
	m.dirty = dirtyFull
}

// CollapseAll collapses every expandable block.
func (m *Model) CollapseAll() {
	for i := range m.items {
		it := &m.items[i]
		if rb := it.ReasBlock; rb != nil {
			for rb.IsExpanded() {
				rb.Toggle()
			}
			it.gen++
		}
		if tb := it.ToolBlock; tb != nil {
			for tb.IsExpanded() {
				tb.Toggle()
			}
			it.gen++
		}
	}
	m.dirty = dirtyFull
}

// CollapseCompacted replaces the first boundary items of the log with a
// one-line stub (yaah-30t6 AC 3): compaction removed those messages
// from the agent's context, so the transcript stops painting them too.
// The window resets because the log shrank, and the prompt index list
// is rebuilt in one pass — the rewrite shifted every index.
func (m *Model) CollapseCompacted(boundary int) {
	if boundary <= 0 || boundary > len(m.items) {
		return
	}
	stub := TextItem(fmt.Sprintf("[%s]· %d messages compacted[-]", m.theme.Dim, boundary))
	rest := append([]Item{stub}, m.items[boundary:]...)
	m.items = rest
	m.rebuildPromptIdxs()
	m.resetWindow()
	m.dirty = dirtyFull
}

// Clear empties the log. The window is stale the moment the log empties:
// recompute at the next repaint.
func (m *Model) Clear() {
	m.items = nil
	m.promptIdxs = nil
	m.userScrolled = false
	m.resetWindow()
	m.dirty = dirtyFull
}

func (m *Model) rebuildPromptIdxs() {
	m.promptIdxs = m.promptIdxs[:0]
	for i := range m.items {
		if m.items[i].IsPrompt {
			m.promptIdxs = append(m.promptIdxs, i)
		}
	}
}

func (m *Model) resetWindow() {
	m.windowStart = -1
	m.windowEnd = -1
}

// Search returns the index of the first item whose content contains the
// lowercased query. The search spans the whole log — not just the
// painted window — because items collapsed behind the seam still exist.
func (m *Model) Search(query string) (int, bool) {
	if query == "" {
		return 0, false
	}
	q := strings.ToLower(query)
	for i := range m.items {
		if strings.Contains(strings.ToLower(m.items[i].SearchText()), q) {
			return i, true
		}
	}
	return 0, false
}

// SearchItem reports whether item i's searchable text contains the
// query, and returns that text.
func (m *Model) SearchItem(i int, query string) (string, bool) {
	text := m.items[i].SearchText()
	q := strings.ToLower(query)
	return text, strings.Contains(strings.ToLower(text), q)
}

// ReportFound renders the ephemeral "found" notice for a search hit.
func (m *Model) ReportFound(k int) string {
	return "Found in message " + strconv.Itoa(k+1) + " of " + strconv.Itoa(len(m.items))
}
