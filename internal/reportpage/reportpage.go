// Package reportpage bounds a report for display without losing it (yaah-okcl).
//
// A dispatch's final report is retained verbatim wherever its owner already
// persists its artifacts — a proposal's sealed metadata, a container
// proposal's meta.json, a background job's record. What reaches a model is a
// PAGE, never the whole thing, and every page states the total byte length so
// a caller always knows how much it has not read and where the next page
// begins.
//
// A page is a half-open byte window snapped to rune boundaries. Both edges
// snap DOWN to the start of a rune, so a page never splits a UTF-8 sequence
// and the end of one page is a legal offset for the next: consecutive pages
// concatenate to the original bytes exactly. Byte offsets (rather than line
// numbers) are what makes that guarantee cheap: a page is one slice, with no
// scan of the text before it.
package reportpage

import (
	"fmt"
	"unicode/utf8"
)

const (
	// DefaultBytes is the default page window. It is the size the dispatch
	// envelope's report head has always used (4000 bytes plus its marker), so
	// paging changed the retrieval route, not the size of a read.
	DefaultBytes = 4000

	// MaxBytes bounds one requested page. A larger page is not a page: a
	// caller that wants 10 MB of report in one read wants a file, and silently
	// honouring it would put the unbounded text back into the context this
	// package exists to protect.
	MaxBytes = 1 << 20
)

// Page is one half-open byte window of a report, snapped to rune boundaries.
// Text holds exactly the bytes of the window; the remaining fields describe
// where the window sits so a caller can render, verify, or continue a read.
type Page struct {
	// Text is the page's bytes, verbatim.
	Text string
	// Offset is the byte offset Text starts at — the requested offset snapped
	// down to a rune boundary, so it can be less than what was asked for.
	Offset int
	// Limit is the page window that was applied (the request, clamped).
	Limit int
	// Total is the full length of the report in bytes.
	Total int
}

// End is the byte offset just past this page.
func (p Page) End() int { return p.Offset + len(p.Text) }

// Truncated reports that the page does not reach the end of the report.
func (p Page) Truncated() bool { return p.End() < p.Total }

// Next is the offset of the byte after this page: the value to pass as the
// next request's offset. It is the end of the page, which is a rune boundary,
// so consecutive pages neither drop nor duplicate bytes.
func (p Page) Next() int { return p.End() }

// Head returns the report's first page.
func Head(s string, limit int) Page { return At(s, 0, limit) }

// At returns the page of s that starts at byte offset and spans at most limit
// bytes.
//
// Bounds are clamped, never refused: a negative offset is 0, an offset past
// the end is the empty page at the end (Total still reports the length, so a
// caller can tell "out of range" from "empty report"), and a non-positive or
// oversized limit falls back to DefaultBytes — never to "everything", because
// an unbounded page is the thing this package exists to avoid.
func At(s string, offset, limit int) Page {
	if limit <= 0 || limit > MaxBytes {
		limit = DefaultBytes
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(s) {
		offset = len(s)
	}
	start := runeStart(s, offset)
	end := start + limit
	if end >= len(s) {
		end = len(s)
	} else {
		end = runeStart(s, end)
		if end <= start {
			// The limit is narrower than the rune at start (report_limit: 1
			// over multi-byte text is legal per the schema). Advance one whole
			// rune so a page is never empty and Next() always exceeds Offset —
			// without this the caller's offset == next loop spins forever
			// (PR #419 review finding 1).
			_, width := utf8.DecodeRuneInString(s[start:])
			end = start + width
		}
	}
	if end == start && start < len(s) {
		// A window narrower than the rune at start would otherwise return an
		// empty page whose Next() equals its Offset, so a caller paging by
		// result_next_offset would loop forever. Advance one whole rune: its
		// start is still a rune boundary, so pages still concatenate exactly.
		end = start + 1
		for end < len(s) && s[end]&0xC0 == 0x80 {
			end++
		}
	}
	return Page{Text: s[start:end], Offset: start, Limit: limit, Total: len(s)}
}

// runeStart moves i back to the start of the rune containing it. A byte that is
// not a UTF-8 continuation byte already starts a rune, so canonical text pays
// nothing for this check.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && s[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

// Marker renders the truncation note a bounded head carries. It states which
// bytes are shown and out of how many, then names the caller's retrieval route
// so the tail is never "somewhere else, find it yourself". An empty route is
// not silence: it says the rest was not retained, because a marker that only
// says "truncated" reads as if a route exists.
func Marker(p Page, route string) string {
	if !p.Truncated() {
		return ""
	}
	if route == "" {
		route = "the rest was not retained; nothing beyond this head is retrievable"
	}
	return fmt.Sprintf("\n...[report truncated: showing bytes %d-%d of %d; %s]", p.Offset, p.End(), p.Total, route)
}
