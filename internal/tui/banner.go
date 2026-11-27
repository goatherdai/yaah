package tui

// banner.go — banner toggle and sizing.

func (t *App) toggleBanner() {
	t.showBanner = !t.showBanner
	if t.showBanner {
		t.Root.RemoveItem(t.Header)
		t.Root.AddItem(t.Header, t.headerHeight(), 0, false)
	} else {
		t.Root.RemoveItem(t.Header)
	}
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
