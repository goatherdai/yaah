// Package rule draws a one-row horizontal separator line in a theme
// color. It exists for the fixed chrome between layout regions — the
// seam under the sticky prompt pin — where a border would change the
// neighbor's geometry and a blank row says nothing.
package rule

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Rule is a tview primitive that paints its inner rect's rows with the
// horizontal box-drawing rune in the configured color.
type Rule struct {
	*tview.Box

	color tcell.Color
}

// New returns a rule painted in color (a theme hex string).
func New(color string) *Rule {
	return &Rule{Box: tview.NewBox(), color: tcell.GetColor(color)}
}

// Draw paints the separator across the inner rect.
func (r *Rule) Draw(screen tcell.Screen) {
	r.Box.DrawForSubclass(screen, r)
	x, y, w, h := r.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}
	style := tcell.StyleDefault.Foreground(r.color)
	for row := 0; row < h; row++ {
		for c := 0; c < w; c++ {
			screen.SetContent(x+c, y+row, '─', nil, style)
		}
	}
}
