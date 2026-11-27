package transcript

import (
	"sort"
	"strings"
	"time"

	"github.com/buchenberg/tviewmd"
	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/messages"
	"github.com/rivo/tview"
)

// layout.go — the row model: Repaint turns the painted window into
// wrapped tagged rows and the prefix/anchor tables, appending only new
// tail items on the fast path and re-wrapping the window otherwise.

// Stats reports one repaint's measured work, for the tui.refresh span.
type Stats struct {
	Items    int
	Rows     int
	Bytes    int
	Appended bool

	WindowStart int
	FullRenders int

	WrapDur     time.Duration
	AssembleDur time.Duration
	TotalDur    time.Duration
}

// Repaint brings the row table up to date at width w. When only new
// items were appended to the log since the last repaint and the painted
// window reaches the live tail, it wraps just those and appends their
// rows; otherwise it re-wraps the painted window (the trailing
// WindowItems/WindowRows of the log, with an "earlier messages" seam
// above it when older items are collapsed). This keeps per-refresh work
// O(new content) during streaming and O(window) on full rebuilds,
// instead of O(entire conversation).
func (m *Model) Repaint(w int) Stats {
	start := time.Now()
	n := len(m.items)

	var stats Stats
	if m.canAppend(w, n) {
		m.appendRows(w, n, &stats)
	} else {
		m.rebuild(w, n, &stats)
	}
	m.dirty = dirtyClean

	stats.Items = n
	stats.Rows = len(m.rows)
	stats.WindowStart = m.windowStart
	stats.FullRenders = m.fullRenders
	m.paintedLines.Store(int64(len(m.rows)))
	m.charsRendered.Store(int64(stats.Bytes))
	stats.TotalDur = time.Since(start)
	return stats
}

// canAppend reports whether the append fast path is eligible: only new
// items at the tail since the last repaint, nothing forced a full
// rebuild, same width, and the window extending to the live tail — a
// reveal window (windowEnd >= 0) must not grow toward the tail one
// append at a time.
func (m *Model) canAppend(w, n int) bool {
	return m.dirty != dirtyFull &&
		m.paintedWidth == w &&
		m.windowEnd < 0 &&
		n > m.end &&
		m.end > 0
}

// appendRows wraps the new tail items and appends their rows, extending
// the prefix and anchor tables in step. Prior rows are untouched.
func (m *Model) appendRows(w, n int, stats *Stats) {
	wrapStart := time.Now()
	firstNew := len(m.rows)
	for i := m.end; i < n; i++ {
		m.prefix = append(m.prefix, len(m.rows))
		it := &m.items[i]
		if it.IsPrompt {
			m.anchors = append(m.anchors, Anchor{Item: i, Row: len(m.rows), Text: it.Prompt})
		}
		m.rows = append(m.rows, m.itemRows(i, w)...)
	}
	stats.WrapDur = time.Since(wrapStart)
	assembleStart := time.Now()
	stats.Bytes = m.rowBytes(firstNew)
	stats.AssembleDur = time.Since(assembleStart)
	m.end = n
	stats.Appended = true
}

// rebuild re-wraps the painted window. windowStart pins the first
// painted item (backfill moves it; -1 means recompute — resize,
// compaction, clear, and return-to-tail reset it so the window tracks
// the budgets). At the live tail the window recomputes on every full
// repaint: a pinned start would let the painted range grow past the
// budgets across a long session and reintroduce the freeze. While the
// user has scrolled up, the manual window is kept so full repaints a
// running turn forces do not yank the content out from under them. A
// reveal window also ends before the tail, bounded by the budgets.
func (m *Model) rebuild(w, n int, stats *Stats) {
	start := m.windowStart
	if !m.userScrolled || start < 0 || start > n || w != m.windowWidth {
		start = m.computeWindowStart(n, w)
		m.windowStart = start
		m.windowWidth = w
		m.windowEnd = -1
	}
	end := n
	if e := m.windowEnd; e >= 0 && e < n {
		end = e
	}

	wrapStart := time.Now()
	m.start, m.end = start, end
	m.paintedWidth = w
	m.rows = m.rows[:0]
	m.prefix = m.prefix[:0]
	m.anchors = m.anchors[:0]
	m.windowPrePrompt = ""
	if j := sort.SearchInts(m.promptIdxs, start); j > 0 {
		m.windowPrePrompt = m.items[m.promptIdxs[j-1]].Prompt
	}
	m.rows = append(m.rows, m.seamTopRows(start, w)...)
	for i := start; i < end; i++ {
		m.prefix = append(m.prefix, len(m.rows))
		it := &m.items[i]
		if it.IsPrompt {
			m.anchors = append(m.anchors, Anchor{Item: i, Row: len(m.rows), Text: it.Prompt})
		}
		m.rows = append(m.rows, m.itemRows(i, w)...)
	}
	m.rows = append(m.rows, m.seamBottomRows(n-end, w)...)
	stats.WrapDur = time.Since(wrapStart)
	assembleStart := time.Now()
	stats.Bytes = m.rowBytes(0)
	stats.AssembleDur = time.Since(assembleStart)
	m.fullRenders++
}

// rowBytes sums the byte length of the painted rows at or after from.
func (m *Model) rowBytes(from int) int {
	b := 0
	for _, r := range m.rows[from:] {
		b += len(r)
	}
	return b
}

// itemRows returns the item's wrapped rows at width w, cached on the
// item. A width change re-wraps wholesale; a block mutation (gen bump)
// re-renders the emission first. The one trailing empty line an item's
// closing newline hands the next item is pending, not painted, and is
// dropped — the same rule the seam lines follow, so every painted row is
// accounted exactly once.
func (m *Model) itemRows(i, w int) []string {
	it := &m.items[i]
	if it.rows != nil && it.rowsWidth == w && it.rowsGen == it.gen && it.rowsLevel == m.level {
		return it.rows
	}
	tagged := m.itemTagged(it, w)
	lines := tview.WordWrap(tagged, w)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	it.rows = lines
	it.rowsWidth = w
	it.rowsGen = it.gen
	it.rowsLevel = m.level
	return lines
}

// itemTagged emits the item's rendered tagged form. Markdown items
// render through RenderMarkdown at display width (cached); the emission
// itself is messages.EmitItem — the single source shared by painting
// and any future text extraction.
func (m *Model) itemTagged(it *Item, w int) string {
	mi := it.Item
	if it.Markdown {
		if it.cached == "" || it.cachedWidth != w || it.cachedLevel != m.level {
			it.cached = RenderMarkdown(it.Text, w, m.theme)
			it.cachedWidth = w
			it.cachedLevel = m.level
		}
		mi.Text = it.cached
	}
	var b strings.Builder
	messages.EmitItem(&b, &mi, colors.RenderCtx{Width: w, Theme: m.theme})
	return b.String()
}

// RenderMarkdown converts markdown to tview color-tagged text. Width
// controls table column sizing. The output uses valid tview tags — no
// raw brackets survive to confuse the tag parser.
func RenderMarkdown(md string, width int, th *colors.Theme) string {
	if md == "" {
		return ""
	}
	if width <= 0 {
		width = 80
	}
	if width > 2 {
		width -= 2
	}
	return tviewmd.Render(md, tviewmd.Options{Width: width, Theme: mdTheme(th)})
}

func mdTheme(th *colors.Theme) tviewmd.Theme {
	return tviewmd.Theme{
		Heading:      th.MDHeading,
		Link:         th.MDLink,
		InlineCodeFG: th.MDCodeFG,
		InlineCodeBG: "",
		CodeBlockFG:  th.MDCodeFG,
		QuoteFG:      th.MDQuoteFG,
		Hr:           th.MDHr,
	}
}
