package transcript

import (
	"fmt"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/tui/colors"
)

// scrollbarTestPane builds a painted model and pane at a fixed size:
// 50 one-line text items at content width 40, viewport 10 rows.
func scrollbarTestPane(t *testing.T, items int) (*Model, *Pane) {
	t.Helper()
	th := colors.NewDarkTheme()
	m := NewModel(&th)
	for i := 0; i < items; i++ {
		m.Append(TextItem(fmt.Sprintf("row %03d", i)))
	}
	p := NewPane(m)
	p.SetRect(0, 0, 41, 10)
	m.Repaint(p.ContentWidth())
	return m, p
}

// TestScrollbarGeom pins the thumb math: a full-height thumb when the
// content fits, a proportional thumb clamped to [1, h] otherwise, the
// thumb at the track's top row at offset 0 and at its bottom row at the
// last offset, and no track at all for degenerate rects.
func TestScrollbarGeom(t *testing.T) {
	_, p := scrollbarTestPane(t, 3)
	rows := len(p.Model().Rows())
	h := 10
	if rows >= h {
		t.Fatalf("fixture rows=%d should fit the %d-row viewport", rows, h)
	}
	g, ok := p.ScrollbarGeom()
	if !ok {
		t.Fatalf("no scrollbar geometry for a healthy rect")
	}
	if g.X != 40 || g.Y != 0 || g.H != h {
		t.Fatalf("track rect = (x=%d, y=%d, h=%d), want (40, 0, %d)", g.X, g.Y, g.H, h)
	}
	if g.ThumbStart != g.Y || g.ThumbSize != h {
		t.Fatalf("fits-content thumb = (start=%d, size=%d), want full-height at Y", g.ThumbStart, g.ThumbSize)
	}

	_, p = scrollbarTestPane(t, 50)
	rows = len(p.Model().Rows())
	if rows <= h {
		t.Fatalf("fixture rows=%d should overflow the %d-row viewport", rows, h)
	}
	p.Model().SetScrolled(true)

	span := rows - h
	size := h * h / rows
	if size < 1 {
		size = 1
	}

	p.ScrollTo(0)
	g, _ = p.ScrollbarGeom()
	if g.ThumbStart != 0 {
		t.Fatalf("thumb start at offset 0 = %d, want 0", g.ThumbStart)
	}

	p.ScrollTo(span)
	g, _ = p.ScrollbarGeom()
	if g.ThumbStart != g.H-size {
		t.Fatalf("thumb start at last offset = %d, want %d", g.ThumbStart, g.H-size)
	}

	// A huge transcript must still leave a grabbable thumb: size clamps
	// at 1 row.
	g, _ = p.ScrollbarGeom()
	if g.ThumbSize < 1 || g.ThumbSize > h {
		t.Fatalf("thumb size %d outside [1, %d]", g.ThumbSize, h)
	}

	// Degenerate rects have no track.
	p.SetRect(0, 0, 1, 10)
	if _, ok := p.ScrollbarGeom(); ok {
		t.Fatalf("1-wide pane claims a scrollbar")
	}
}

// TestScrollbarPaint pins the drawn chrome: the track column holds the
// trough rune on every viewport row, the thumb's rows hold the thumb
// rune, and content never prints under the track.
func TestScrollbarPaint(t *testing.T) {
	_, p := scrollbarTestPane(t, 50)
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer sim.Fini()
	sim.SetSize(41, 10)
	p.Model().SetScrolled(true)
	p.ScrollTo(0)
	p.Draw(sim)
	sim.Show()

	g, ok := p.ScrollbarGeom()
	if !ok {
		t.Fatalf("no scrollbar geometry")
	}
	cells, _, _ := sim.GetContents()
	cell := func(x, y int) rune {
		if len(cells[y*41+x].Runes) == 0 {
			return ' '
		}
		return cells[y*41+x].Runes[0]
	}
	for r := 0; r < g.H; r++ {
		want := scrollbarTrackRune
		if r >= g.ThumbStart && r < g.ThumbStart+g.ThumbSize {
			want = scrollbarThumbRune
		}
		if got := cell(g.X, g.Y+r); got != want {
			t.Fatalf("track row %d painted %q, want %q", r, got, want)
		}
	}
	// Chrome and content share the first row cleanly: the viewport's
	// top row is the emission's blank separator in every content
	// column, while the track column still carries the trough rune.
	for c := 0; c < g.X; c++ {
		if got := cell(c, g.Y); got != ' ' {
			t.Fatalf("separator row column %d painted %q, want blank", c, got)
		}
	}
}

// TestScrollToThumb pins the drag seam's mapping: the ends are exact
// (track top → first row, track bottom → last row), interior track
// rows land on their quantum — a coarse thumb cannot represent every
// offset, so row k maps to floor(k·span/(H−T)) — and values beyond
// the track's ends clamp.
func TestScrollToThumb(t *testing.T) {
	_, p := scrollbarTestPane(t, 50)
	p.Model().SetScrolled(true)
	rows := len(p.Model().Rows())
	h := 10
	span := rows - h
	g, _ := p.ScrollbarGeom()

	p.ScrollToThumb(g.Y)
	if got := p.Offset(); got != 0 {
		t.Fatalf("ScrollToThumb at track top = %d, want 0", got)
	}
	p.ScrollToThumb(g.Y + g.H - 1)
	if got := p.Offset(); got != span {
		t.Fatalf("ScrollToThumb at track bottom = %d, want %d", got, span)
	}
	for rel := 0; rel <= g.H-g.ThumbSize; rel++ {
		p.ScrollToThumb(g.Y + rel)
		want := rel * span / (g.H - g.ThumbSize)
		if got := p.Offset(); got != want {
			t.Fatalf("ScrollToThumb at track row %d = %d, want %d", rel, got, want)
		}
	}
	p.ScrollToThumb(-100)
	if got := p.Offset(); got != 0 {
		t.Fatalf("ScrollToThumb above the track = %d, want 0", got)
	}
	p.ScrollToThumb(1000)
	if got := p.Offset(); got != span {
		t.Fatalf("ScrollToThumb below the track = %d, want %d", got, span)
	}
}

// TestScrollToThumbOneRowViewport pins the degenerate drag: a 1-row
// viewport over overflowing content clamps the thumb to the whole
// track, which has no representable positions — ScrollToThumb must be
// a no-op, not a divide-by-zero.
func TestScrollToThumbOneRowViewport(t *testing.T) {
	_, p := scrollbarTestPane(t, 50)
	p.SetRect(0, 0, 11, 1)
	p.Model().SetScrolled(true)
	p.ScrollTo(5)
	g, ok := p.ScrollbarGeom()
	if !ok || g.H != 1 || g.ThumbSize != 1 {
		t.Fatalf("1-row viewport geometry = (ok=%v h=%d size=%d), want full-track thumb", ok, g.H, g.ThumbSize)
	}
	p.ScrollToThumb(g.Y)
	if got := p.Offset(); got != 5 {
		t.Fatalf("no-position drag moved the viewport to %d, want it pinned at 5", got)
	}
}

// TestContentWidthReservesTrack pins the wrap-width contract: the track
// column is excluded from the width items wrap at, unconditionally, so
// the wrap width never changes when content becomes scrollable — and
// the no-rect fallback honors the same contract.
func TestContentWidthReservesTrack(t *testing.T) {
	_, p := scrollbarTestPane(t, 1)
	if got := p.ContentWidth(); got != 40 {
		t.Fatalf("41-wide pane content width = %d, want 40", got)
	}
	p.SetRect(0, 0, 1, 10)
	if got := p.ContentWidth(); got != 1 {
		t.Fatalf("1-wide pane content width = %d, want 1", got)
	}
	p.SetRect(0, 0, 0, 10)
	if got := p.ContentWidth(); got != 79 {
		t.Fatalf("no-rect content width = %d, want the 79 fallback", got)
	}
}
