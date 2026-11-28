// Package transcript owns the TUI's conversation transcript: the item
// log, the painted window over it, the wrapped row model that turns
// items into screen rows, and the pane primitive that paints those rows
// onto a tcell screen.
//
// The Model replaces the single messages TextView: items are wrapped
// once into tagged rows at the painted width, a prefix table maps every
// painted item to its exact first row (prompt anchors, search reveals,
// and scroll targets read it directly instead of re-measuring), and the
// Pane draws only the visible viewport's rows per frame. tcell's own
// double-buffering diffs the emitted cells against the terminal.
//
// Concurrency: Model and Pane are UI-goroutine state. Every method must
// run on the tview event-loop goroutine (a QueueUpdateDraw callback),
// with one documented exception: seeding before App.Run, where no
// queued callback can race the seed (see RestoreHistory callers).
package transcript

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/messages"
	"github.com/goatherdai/yaah/internal/tui/components/reasoning"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
	"github.com/goatherdai/yaah/internal/tui/lolcat"
	"github.com/rivo/tview"
)

// Window budgets (T2 semantics, unchanged):
const (
	// WindowItems caps how many trailing conversation items a full render
	// paints (hermes MAX_MOUNTED=120): beyond the cap the older items
	// collapse behind the "earlier messages" seam and backfill on scroll.
	WindowItems = 120

	// WindowRows caps the painted buffer's row count for the same purpose:
	// a window of few-but-huge items must not paint a freeze-sized buffer.
	WindowRows = 2000

	// BackfillStep is how many older items one scroll-to-the-seam prepends
	// (hermes SLIDE_STEP=12).
	BackfillStep = 12
)

// Item is one conversation entry in the transcript log. Exactly one
// payload field is set (see messages.Item); identity and cache fields
// are the transcript's own.
type Item struct {
	messages.Item

	// IsPrompt marks a user question; Prompt keeps the raw input for the
	// sticky header pin and search.
	IsPrompt bool
	Prompt   string

	// Markdown marks assistant text: it renders through RenderMarkdown at
	// display width instead of painting the raw text.
	Markdown bool

	// gen bumps whenever the item's block mutates in place (tool end or
	// error, expand toggles), so the wrap and markdown caches re-render it.
	gen int

	// Markdown render cache: keyed on width and zoom level (the render is
	// a pure function of the immutable text).
	cached      string
	cachedWidth int
	cachedLevel int

	// Wrapped-rows cache: keyed on width, zoom level, and gen (a mutated
	// block re-wraps).
	rows      []string
	rowsWidth int
	rowsGen   int
	rowsLevel int
}

// SearchText returns the item's content in searchable form: the
// name/args/result for tool blocks, the content for reasoning blocks,
// raw text for markdown items, and tag-stripped text for chrome items
// (prompts, notices). Markdown stays raw because tag-stripping would eat
// markdown brackets such as [label](url) and hide the label.
func (it *Item) SearchText() string {
	switch {
	case it.ToolBlock != nil:
		return it.ToolBlock.SearchText()
	case it.ReasBlock != nil:
		return it.ReasBlock.SearchText()
	case it.Markdown:
		return it.Text
	default:
		return lolcat.StripTags(it.Text)
	}
}

// Anchor records a prompt item's exact first painted row in the current
// layout.
type Anchor struct {
	Item int
	Row  int
	Text string
}

// dirtyKind classifies what changed since the last repaint: nothing, new
// tail items only (the append fast path re-wraps just those), or
// anything else (the window rebuild re-wraps the painted window).
type dirtyKind int

const (
	dirtyClean dirtyKind = iota
	dirtyAppended
	dirtyFull
)

// Model owns the conversation log and everything derived from it for
// painting: the painted window, the wrapped row table, the prompt
// anchors, and the render counters. It is the single owner of what the
// transcript shows; the Pane decides only where the viewport sits.
type Model struct {
	theme *colors.Theme

	items []Item
	// promptIdxs lists the indices of prompt items in ascending order,
	// maintained by Append and rebuilt by the log rewrites (compaction).
	// The anchor rebuild finds the last prompt above the painted window
	// with one binary search over it.
	promptIdxs []int

	// Painted window (T2 semantics). windowStart is the first item a full
	// render paints; -1 recomputes the trailing window at the next full
	// repaint (resize, compaction, clear, and returning to the live tail
	// reset it). windowWidth is the width the window was computed at, so a
	// width change re-windows instead of reusing stale row budgets.
	// windowEnd is the exclusive end; -1 extends to the live tail. Only
	// reveal (search) installs a bounded end. userScrolled marks manual
	// window mode so running-turn repaints do not yank the content.
	windowStart     int
	windowWidth     int
	windowEnd       int
	userScrolled    bool
	windowPrePrompt string

	// dirty classifies the change since the last repaint.
	dirty dirtyKind

	// layout is the painted window's row table from the last Repaint.
	// start/end are the painted item range [start,end); rows holds the
	// seam rows and every painted item's wrapped rows in order; prefix
	// maps painted item i to its first row via prefix[i-start]; anchors
	// list the prompt items' exact rows.
	start        int
	end          int
	rows         []string
	prefix       []int
	anchors      []Anchor
	paintedWidth int

	// level is the zoom level, part of every render cache key from day
	// one so the zoom ladder can land without re-keying the caches.
	// Zero today; no path sets it yet.
	level int

	fullRenders int

	// unmatchedEnds is the bounded ring of tool end events that settled no
	// block (diagnostic only; see SettleTool).
	unmatchedEnds []UnmatchedToolEnd

	paintedLines  atomic.Int64
	charsRendered atomic.Int64
}

// NewModel returns an empty transcript rendered with th.
func NewModel(th *colors.Theme) *Model {
	return &Model{
		theme:       th,
		windowStart: -1,
		windowEnd:   -1,
	}
}

// Item constructors. Appends go through Append, which maintains
// promptIdxs; the constructors exist so callers cannot build an Item in
// an invalid cache state.

// PromptItem builds a user-question item. display is the prompt's
// rendered transcript text; input is kept raw for the sticky pin.
func PromptItem(display, input string) Item {
	return Item{
		Item:     messages.Item{Text: display},
		IsPrompt: true,
		Prompt:   input,
	}
}

// TextItem builds a chrome item whose text already carries its tags
// (notices, markers).
func TextItem(text string) Item {
	return Item{Item: messages.Item{Text: text}}
}

// MarkdownItem builds an assistant-response item: raw markdown, rendered
// at display width.
func MarkdownItem(md string) Item {
	return Item{Item: messages.Item{Text: md}, Markdown: true}
}

// ToolItem builds a tool-call item around its block.
func ToolItem(tb *toolblock.Block) Item {
	return Item{Item: messages.Item{ToolBlock: tb}}
}

// ReasoningItem builds a reasoning item around its block.
func ReasoningItem(rb *reasoning.Block) Item {
	return Item{Item: messages.Item{ReasBlock: rb}}
}

// Append adds it to the log tail. New tail items keep the append fast
// path eligible; nothing repaints — the caller schedules that.
func (m *Model) Append(it Item) {
	m.items = append(m.items, it)
	if it.IsPrompt {
		m.promptIdxs = append(m.promptIdxs, len(m.items)-1)
	}
	if m.dirty == dirtyClean {
		m.dirty = dirtyAppended
	}
}

// RequestFull forces the next repaint to rebuild the window instead of
// appending. Public for the driver (history seeding) and tests; the
// in-place mutators call it internally.
func (m *Model) RequestFull() {
	m.dirty = dirtyFull
}

// Len reports the item count.
func (m *Model) Len() int { return len(m.items) }

// ItemAt returns item i by value; block mutations go through the
// mutator methods, which keep the caches honest.
func (m *Model) ItemAt(i int) Item { return m.items[i] }

// PromptIndices returns the prompt items' log indices.
func (m *Model) PromptIndices() []int { return m.promptIdxs }

// WindowStart reports the first painted item's index (-1 until the first
// full repaint).
func (m *Model) WindowStart() int { return m.windowStart }

// PaintedWidth reports the width the current row table was wrapped at.
// The draw seam compares it against the pane's live width to catch
// resizes: rows do not re-wrap themselves, so a width change must
// schedule a repaint.
func (m *Model) PaintedWidth() int { return m.paintedWidth }

// WindowEnd reports the exclusive painted end (-1 at the live tail).
func (m *Model) WindowEnd() int { return m.windowEnd }

// UserScrolled reports whether the user has manually scrolled (manual
// window mode).
func (m *Model) UserScrolled() bool { return m.userScrolled }

// SetScrolled marks manual-scroll mode: full repaints keep the manual
// window instead of recomputing the trailing one.
func (m *Model) SetScrolled(v bool) { m.userScrolled = v }

// Anchors returns the current prompt anchor table.
func (m *Model) Anchors() []Anchor { return m.anchors }

// PreWindowPrompt returns the most recent prompt entirely above the
// painted window, so the sticky pin survives the seam.
func (m *Model) PreWindowPrompt() string { return m.windowPrePrompt }

// FullRenders counts repaints that rebuilt the window.
func (m *Model) FullRenders() int { return m.fullRenders }

// PaintedLines returns the painted row count (telemetry).
func (m *Model) PaintedLines() int64 { return m.paintedLines.Load() }

// CharsRendered returns the painted byte count (telemetry).
func (m *Model) CharsRendered() int64 { return m.charsRendered.Load() }

// Rows returns the painted row table (tagged lines, seams included).
// The returned slice is owned by the model; callers must not retain or
// mutate it across repaints.
func (m *Model) Rows() []string { return m.rows }

// PaintedText returns the painted rows joined with newlines. Tag
// markers are retained; the string is the painted buffer's raw form,
// for tests and future text extraction (copy).
func (m *Model) PaintedText() string { return strings.Join(m.rows, "\n") }

// ItemRow returns item i's first painted row, reporting false when the
// item sits outside the painted window instead of a misleading row 0 —
// callers holding stale windows must repaint first. Reveal guarantees
// its hits are inside the window (its budget yields before the hit).
func (m *Model) ItemRow(i int) (int, bool) {
	if i < m.start || i >= m.end {
		return 0, false
	}
	return m.prefix[i-m.start], true
}

// PinFor returns the prompt to show in the sticky header when the
// viewport's top row is row. A prompt takes the pin only once its first
// painted row has scrolled past the viewport top, so the in-stream copy
// at the top row and the pin never double-render (yaah-983). Above the
// painted window the last pre-window prompt takes over, so the pin
// survives the seam in every shape.
func (m *Model) PinFor(row int) string {
	want := ""
	if m.start > 0 {
		want = m.windowPrePrompt
	}
	if n := len(m.anchors); n > 0 {
		// The last anchor strictly above the viewport top.
		i := sort.Search(n, func(i int) bool { return m.anchors[i].Row >= row })
		if i > 0 {
			want = m.anchors[i-1].Text
		}
	}
	return want
}

// seamTopText renders the dim "older messages" line painted above the
// window when earlier items are collapsed behind it.
func (m *Model) seamTopText(older int) string {
	return fmt.Sprintf("[%s]· %d earlier messages — scroll up to load more ·[-]",
		m.theme.Dim, older)
}

// seamBottomText renders the dim "later messages" line painted below a
// reveal window that ends before the live tail.
func (m *Model) seamBottomText(later int) string {
	return fmt.Sprintf("[%s]· %d later messages — scroll down for the live tail ·[-]",
		m.theme.Dim, later)
}

// seamTopRows and seamBottomRows report the seam lines' wrapped rows at
// width w, so seam accounting is exact like every other row.
func (m *Model) seamTopRows(start, w int) []string {
	if start <= 0 {
		return nil
	}
	return m.wrapLine(m.seamTopText(start), w)
}

func (m *Model) seamBottomRows(later, w int) []string {
	if later <= 0 {
		return nil
	}
	return m.wrapLine(m.seamBottomText(later), w)
}

// wrapLine wraps a standalone tagged line at width w. A lone trailing
// empty segment is the pending row a trailing newline would hand the
// next item; standalone seam lines carry no trailing newline, and
// dropping the segment keeps the accounting shared with item rows.
func (m *Model) wrapLine(tagged string, w int) []string {
	lines := tview.WordWrap(tagged, w)
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
