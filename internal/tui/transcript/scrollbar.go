package transcript

import (
	"github.com/gdamore/tcell/v2"
)

// scrollbar.go — the conversation pane's scrollbar. The track reserves
// the pane inner rect's rightmost column (so the wrap width is stable
// whether or not content is scrollable), and the thumb is honest to the
// painted window: its extent covers the model's wrapped-row table only,
// never the items still collapsed behind the "earlier messages" seam.
// Reaching the top is the same progressive-disclosure contract as the
// wheel: the caller backfills, the row table grows, and the thumb
// shrinks — the scrollbar never claims knowledge of unpainted rows.

// ScrollbarGeom is the scrollbar's geometry in screen coordinates:
// the track's column and vertical extent, plus the thumb's first row
// and size within the track. All values are re-derived from the
// offset, the row table, and the last drawn viewport height on every
// call — there is no cached geometry to fall out of step with a
// resize, a backfill, or a streaming append.
type ScrollbarGeom struct {
	// X is the track's screen column; Y..Y+H-1 its rows.
	X, Y, H int
	// ThumbStart is the thumb's first screen row; ThumbSize its height
	// in rows, always >= 1 and <= H.
	ThumbStart int
	ThumbSize  int
}

// Scrollbar runes: a light trough and a solid thumb.
const (
	scrollbarTrackRune = '│'
	scrollbarThumbRune = '┃'
)

// SetScrollbarColors sets the track and thumb foreground colors.
// Defaults are terminal gray and white; the app wires the theme's
// colors (NoColor included) in at construction.
func (p *Pane) SetScrollbarColors(track, thumb tcell.Color) {
	p.trackColor, p.thumbColor = track, thumb
}

// ScrollbarGeom returns the scrollbar's current geometry. ok is false
// when the pane has no room for a track (degenerate rect) — the caller
// treats the click as ordinary content space.
func (p *Pane) ScrollbarGeom() (g ScrollbarGeom, ok bool) {
	_, y, w, h := p.GetInnerRect()
	if w < 2 || h <= 0 {
		return ScrollbarGeom{}, false
	}
	g = ScrollbarGeom{X: p.trackX(w), Y: y, H: h}
	rows := len(p.model.Rows())
	switch span := rows - h; {
	case span <= 0:
		// Content fits: a full-height thumb states "everything is
		// visible" instead of hiding the affordance.
		g.ThumbStart = y
		g.ThumbSize = h
	default:
		g.ThumbSize = max(1, h*h/rows)
		g.ThumbStart = y + p.offset*(h-g.ThumbSize)/span
	}
	return g, true
}

// trackX returns the track's screen column for inner width w: the
// rightmost inner column, which Draw leaves to the scrollbar and
// ContentWidth excludes from the wrap width.
func (p *Pane) trackX(w int) int {
	x, _, _, _ := p.GetInnerRect()
	return x + w - 1
}

// ScrollToThumb moves the viewport so the thumb's top row lands on sy
// (screen space): the drag path's absolute positioning. Values outside
// the track clamp to its ends.
func (p *Pane) ScrollToThumb(sy int) {
	g, ok := p.ScrollbarGeom()
	if !ok {
		return
	}
	rel := clamp(sy-g.Y, 0, g.H-g.ThumbSize)
	span := len(p.model.Rows()) - g.H
	if span <= 0 {
		p.offset = 0
		return
	}
	// A thumb that fills the track (content fits, or a 1-row viewport
	// over any overflow) has no representable positions: the drag is a
	// no-op, never a divide-by-zero.
	if g.H == g.ThumbSize {
		return
	}
	p.offset = rel * span / (g.H - g.ThumbSize)
	p.ClampOffset()
}

// drawScrollbar paints the track column: the trough in the track style
// and the thumb rows in the thumb style. Both ride the pane's captured
// background, like the content blank.
func (p *Pane) drawScrollbar(screen tcell.Screen, g ScrollbarGeom) {
	bg := p.GetBackgroundColor()
	track := tcell.StyleDefault.Foreground(p.trackColor).Background(bg)
	thumb := tcell.StyleDefault.Foreground(p.thumbColor).Background(bg)
	for r := 0; r < g.H; r++ {
		sy := g.Y + r
		style, ch := track, scrollbarTrackRune
		if sy >= g.ThumbStart && sy < g.ThumbStart+g.ThumbSize {
			style, ch = thumb, scrollbarThumbRune
		}
		screen.SetContent(g.X, sy, ch, nil, style)
	}
}

// clamp constrains v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
