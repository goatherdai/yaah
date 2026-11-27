package tui

// banner.go — banner toggle and sizing.

// toggleBanner flips the header in and out of the layout by resizing it:
// tview's Flex.AddItem appends, so removing the header on hide and
// re-adding it on show would land it after the composer at the root's
// end. A zero-height item paints nothing and keeps its slot.
func (t *App) toggleBanner() {
	t.showBanner = !t.showBanner
	h := 0
	if t.showBanner {
		h = t.headerHeight()
	}
	t.Root.ResizeItem(t.Header, h, 0)
}

// headerHeight is the header grid's row count: the banner's lines plus
// the blank padding row beneath the tagline. The padding lives inside
// the grid so :banner removes it together with the art.
func (t *App) headerHeight() int {
	if !t.showBanner {
		return 0
	}
	return t.bannerLines + 1
}
