package transcript

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Pane is the transcript's tview Primitive: it paints the model's rows
// inside its rect and owns the viewport (the scroll offset). It owns
// nothing else — which rows exist, and where items sit inside them, is
// the Model's truth, read fresh on every frame.
//
// Drawing is viewport-bounded: each frame fills its rect and prints only
// the visible rows via tview.Print, the same tag-aware printer
// tview.TextView uses per line, so painted styles match the previous
// TextView by construction. Cell-level diffing is tcell's job — its
// screen is double-buffered, and Show emits only changed cells to the
// terminal.
//
// One constraint inherited from the Print seam: style-tag backgrounds are
// ignored (Print maintains the screen background). No transcript
// emission uses background tags today; the tool-block and markdown
// renderers emit foreground and attribute tags only.
type Pane struct {
	*tview.Box

	model *Model

	// offset is the viewport's top row within the model's row table.
	// height is the last drawn viewport height, for page scrolling and
	// bottom detection before the first draw.
	offset int
	height int
}

// NewPane returns a pane painting m. The rect is seeded at the
// ContentWidth fallback size: tview's zero-layout Box default is an
// arbitrary 15x10 stub, and a repaint before the first layout would
// wrap the model's rows at that stub width.
func NewPane(m *Model) *Pane {
	p := &Pane{Box: tview.NewBox(), model: m}
	p.SetRect(0, 0, 80, 24)
	return p
}

// Model returns the painted transcript.
func (p *Pane) Model() *Model { return p.model }

// ContentWidth returns the pane's inner width, defaulting to 80 before
// the first layout — the width items are wrapped at.
func (p *Pane) ContentWidth() int {
	_, _, w, _ := p.GetRect()
	if w <= 0 {
		return 80
	}
	return w
}

// Draw paints the visible viewport. While the model follows its live
// tail (UserScrolled false) the viewport re-snaps to the row table's
// end here rather than at repaint time: the pane's height is only
// final once the flex lays it out, and a mid-frame chrome change (the
// sticky pin's rows appearing or clearing) would otherwise leave the
// tail cut off by the stale-height offset until the next repaint. The
// offset is clamped otherwise, so a shrunken row table (clear,
// compaction) can never leave the viewport past the end.
func (p *Pane) Draw(screen tcell.Screen) {
	p.Box.DrawForSubclass(screen, p)
	x, y, w, h := p.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}
	p.height = h
	rows := p.model.Rows()
	if !p.model.UserScrolled() {
		if p.offset = len(rows) - h; p.offset < 0 {
			p.offset = 0
		}
	} else {
		p.ClampOffset()
	}

	blank := tcell.StyleDefault.
		Foreground(tview.Styles.PrimaryTextColor).
		Background(tview.Styles.PrimitiveBackgroundColor)
	for r := 0; r < h; r++ {
		sy := y + r
		for c := 0; c < w; c++ {
			screen.SetContent(x+c, sy, ' ', nil, blank)
		}
		if idx := p.offset + r; idx < len(rows) {
			tview.Print(screen, rows[idx], x, sy, w, tview.AlignLeft, tview.Styles.PrimaryTextColor)
		}
	}
}

// Offset returns the viewport's top row.
func (p *Pane) Offset() int { return p.offset }

// ClampOffset keeps the viewport inside the row table.
func (p *Pane) ClampOffset() {
	max := len(p.model.Rows()) - p.height
	if max < 0 {
		max = 0
	}
	if p.offset > max {
		p.offset = max
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

// ScrollTo moves the viewport's top row.
func (p *Pane) ScrollTo(row int) {
	p.offset = row
	p.ClampOffset()
}

// ScrollToBeginning moves the viewport to the first row.
func (p *Pane) ScrollToBeginning() { p.ScrollTo(0) }

// ScrollToEnd moves the viewport to the last full page.
func (p *Pane) ScrollToEnd() {
	p.offset = len(p.model.Rows())
	p.ClampOffset()
}

// ScrollBy moves the viewport by n rows (negative scrolls up).
func (p *Pane) ScrollBy(n int) {
	p.ScrollTo(p.offset + n)
}

// PageUp and PageDown scroll by the last drawn viewport height.
func (p *Pane) PageUp()   { p.ScrollBy(-max(1, p.height)) }
func (p *Pane) PageDown() { p.ScrollBy(max(1, p.height)) }

// Shift moves the viewport by n rows without clamping to the old row
// table first — backfill uses it after a repaint added rows above the
// viewport, so the content the user was looking at stays put.
func (p *Pane) Shift(n int) {
	p.offset += n
	p.ClampOffset()
}

// AtTop reports whether the viewport sits at the first painted row — the
// seam, where backfill triggers.
func (p *Pane) AtTop() bool { return p.offset <= 0 }

// AtBottom reports whether the viewport sits at the last row of the
// painted window: mouse-scroll-down there returns the view to the live
// tail.
func (p *Pane) AtBottom() bool {
	rows := len(p.model.Rows())
	_, _, _, h := p.GetInnerRect()
	if h <= 0 {
		h = p.height
	}
	return p.offset+max(1, h) >= rows
}
