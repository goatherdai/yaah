package transcript

// window.go — painted-window mechanics: trailing-window computation,
// lazy backfill at the seam, search reveal, and the return to the live
// tail. All row math reads the wrapped-rows cache, so budgets, seams,
// and scroll targets are exact rather than estimated.

// computeWindowStart returns the first item a full render should paint:
// the whole log when it fits the budgets, else the trailing window that
// does. Rows are accumulated tail-first over the per-item rows cache, so
// the walk costs O(window), never O(transcript).
func (m *Model) computeWindowStart(n, w int) int {
	rows, count, start := 0, 0, 0
	for k := n - 1; k >= 0; k-- {
		r := len(m.itemRows(k, w))
		if count > 0 && (count+1 > WindowItems || rows+r > WindowRows) {
			start = k + 1
			break
		}
		rows += r
		count++
	}
	return start
}

// computeWindowEnd returns the exclusive end of a window starting at
// start: forward over the per-item rows cache until the item or row
// budget binds, mirroring computeWindowStart's budgets. Always includes
// start, so end > start for a non-empty log. Callers treat end == n as
// the live tail.
func (m *Model) computeWindowEnd(start, n, w int) int {
	rows, end := 0, start
	for end < n && end-start < WindowItems {
		r := len(m.itemRows(end, w))
		if end > start && rows+r > WindowRows {
			break
		}
		rows += r
		end++
	}
	return end
}

// Backfill prepends BackfillStep older items to the painted window —
// the lazy "scroll up to load more" affordance at the seam. The batch is
// capped by rows as well as items: a dozen tall tool or markdown items
// must not repaint a buffer far past the row budget on the exact scroll
// path meant to stay bounded. The first item always loads, so a seam
// exists whenever progress is possible; an item taller than the whole
// budget alone is simply not lazy-loadable. The manual window survives
// the repaint at the current width: a stale windowWidth would make the
// repaint recompute the trailing window and yank the user to the live
// tail mid-backfill. A reveal window slides its end with the start so
// the painted range stays inside the budgets.
//
// It returns the number of rows inserted above the previously painted
// window's first row — the scroll shift that keeps the content the user
// was looking at in place — or 0 when no older items remain.
func (m *Model) Backfill(w int) int {
	oldStart := m.windowStart
	if oldStart <= 0 {
		return 0
	}
	n := len(m.items)
	newStart, addedRows := oldStart, 0
	for newStart > 0 && oldStart-newStart < BackfillStep {
		r := len(m.itemRows(newStart-1, w))
		if oldStart-newStart > 0 && addedRows+r > WindowRows {
			break
		}
		newStart--
		addedRows += r
	}
	if newStart == oldStart {
		return 0
	}
	oldSeam := len(m.seamTopRows(oldStart, w))
	newSeam := len(m.seamTopRows(newStart, w))
	m.windowStart = newStart
	m.windowWidth = w
	if m.windowEnd >= 0 {
		if end := m.computeWindowEnd(newStart, n, w); end < n {
			m.windowEnd = end
		} else {
			m.windowEnd = -1
		}
	}
	m.dirty = dirtyFull
	return addedRows + newSeam - oldSeam
}

// Reveal installs a bounded window around item k — a search hit. The
// window keeps one backfill batch of context above the hit and runs
// forward only to the item/row budgets — never to the live tail, which
// would repaint an unbounded range for an early hit (the freeze the
// window exists to prevent); a bottom seam reports the collapsed later
// items. The view is marked scrolled so later appends do not yank the
// hit away. The next repaint paints the window; ItemRow(k) then returns
// the hit's exact first row.
func (m *Model) Reveal(k int, w int) {
	n := len(m.items)
	if k < 0 || k >= n {
		return
	}
	m.windowStart = max(0, k-BackfillStep)
	m.windowWidth = w
	end := m.computeWindowEnd(m.windowStart, n, w)
	if end <= k {
		// A single item taller than the row budget inside the backfill
		// batch can stop the forward walk before the hit; the window
		// must still contain the item it exists to show, so the budget
		// yields before the hit does.
		end = k + 1
	}
	if end < n {
		m.windowEnd = end
	} else {
		m.windowEnd = -1
	}
	m.userScrolled = true
	m.dirty = dirtyFull
}

// ReturnToTail returns the view to the live-tail window. The plain reset
// (userScrolled cleared, window recomputed at the next full repaint) is
// enough while the painted buffer still ends at the tail; a reveal
// window ends at its bottom seam instead, so the tail must be repainted
// before it can be shown. It reports whether a repaint is required.
func (m *Model) ReturnToTail() bool {
	m.userScrolled = false
	m.windowStart = -1
	if m.windowEnd >= 0 {
		m.windowEnd = -1
		m.dirty = dirtyFull
		return true
	}
	return false
}
