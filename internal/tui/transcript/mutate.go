package transcript

import (
	"fmt"
	"strconv"
	"strings"
)

// mutate.go — in-place item mutations, log rewrites, and search. Every
// mutation that can change a painted item's row count bumps the item's
// gen (invalidating its caches) and marks the model for a full repaint.

// CompleteTool transitions the tool block with the given ID to its
// completed state and reports whether a block was found.
func (m *Model) CompleteTool(id, summary, result string) bool {
	for i := range m.items {
		if tb := m.items[i].ToolBlock; tb != nil && tb.ID() == id {
			tb.Complete(summary, result)
			m.items[i].gen++
			m.dirty = dirtyFull
			return true
		}
	}
	return false
}

// FailTool transitions the tool block with the given ID to its error
// state and reports whether a block was found.
func (m *Model) FailTool(id, summary, err string) bool {
	for i := range m.items {
		if tb := m.items[i].ToolBlock; tb != nil && tb.ID() == id {
			tb.Fail(summary, err)
			m.items[i].gen++
			m.dirty = dirtyFull
			return true
		}
	}
	return false
}

// SweepRunningTools seals every still-Running tool block as
// interrupted. The turn is over when the sweep runs (the agent loop's
// DoneEvent), so a Running block's end event was dropped or reordered —
// without the sweep it renders a permanent chevron. It reports how many
// blocks it sealed.
func (m *Model) SweepRunningTools() int {
	n := 0
	for i := range m.items {
		if tb := m.items[i].ToolBlock; tb != nil && tb.IsRunning() {
			tb.Interrupt()
			m.items[i].gen++
			n++
		}
	}
	if n > 0 {
		m.dirty = dirtyFull
	}
	return n
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
