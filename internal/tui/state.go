// state.go — focus state and query helpers.
package tui

import (
	"fmt"
	"strings"

	"github.com/goatherdai/yaah/internal/tui/lolcat"
)

type focusState int

const (
	focusNormal focusState = iota
	focusCommandPalette
	focusModal
)

// searchMessages finds the first conversation item whose content matches
// the query and reveals it. The search runs over the whole conversation
// log — not just the painted window — because items collapsed behind the
// "earlier messages" seam still exist; a hit above the window expands it
// so the match is painted and scrolled to.
func (t *App) searchMessages(query string) {
	if query == "" {
		return
	}
	q := strings.ToLower(query)
	for k := range t.conversationLog {
		if strings.Contains(strings.ToLower(t.searchableItemText(&t.conversationLog[k])), q) {
			t.revealItem(k)
			return
		}
	}
	t.SetEphemeral("No matches found")
}

// searchableItemText returns an item's content in searchable form: the
// name/args/result for tool blocks, the content for reasoning blocks,
// raw text for markdown items, and tag-stripped text for chrome items
// (prompts, notices). Markdown is returned raw because StripTags would
// eat markdown brackets such as [label](url) and hide the label; the
// cost is one pass over stored text, not a re-render of painted
// markdown.
func (t *App) searchableItemText(ci *convItem) string {
	switch {
	case ci.toolBlock != nil:
		return ci.toolBlock.SearchText()
	case ci.reasoningBlock != nil:
		return ci.reasoningBlock.SearchText()
	case ci.isMarkdown:
		return ci.text
	default:
		return lolcat.StripTags(ci.text)
	}
}

// revealItem ensures item k is painted and scrolls the viewport to its
// estimated first row. A collapsed item (above the window) expands the
// window first, keeping one backfill batch of context above the hit; the
// row estimate uses itemRows, the same measure the prompt anchors use.
func (t *App) revealItem(k int) {
	w := messageWidth(t.Messages)
	// windowStart -1 means no window is established yet (fresh session,
	// live-tail return, compaction, clear): normalize to the natural
	// trailing window before the paint decision. Without this, a hit
	// inside the painted range skips the refresh and the row loop
	// below starts at -1, indexing conversationLog[-1].
	if t.windowStart < 0 {
		t.windowStart = t.computeWindowStart(len(t.conversationLog), w)
		t.windowWidth = w
	}
	if k < t.windowStart || k >= t.renderedItems {
		if k < t.windowStart {
			t.windowStart = max(0, k-renderBackfillStep)
			t.windowWidth = w
		}
		t.userScrolled = true
		t.needsFullRender.Store(true)
		t.refreshMessages()
	}
	rows := t.seamRows(t.windowStart, w)
	for i := t.windowStart; i < k; i++ {
		rows += t.itemRows(&t.conversationLog[i], t.buildItems(i, i+1, w)[0], w)
	}
	t.Messages.ScrollTo(rows, 0)
	t.SetEphemeral(fmt.Sprintf("Found in message %d of %d", k+1, len(t.conversationLog)))
}
