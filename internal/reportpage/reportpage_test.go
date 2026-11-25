package reportpage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAt_WindowIsExactAndBounded(t *testing.T) {
	report := strings.Repeat("abcdefghij", 1000) // 10000 bytes
	page := At(report, 0, DefaultBytes)
	if page.Total != 10000 {
		t.Fatalf("Total = %d, want 10000", page.Total)
	}
	if len(page.Text) != DefaultBytes {
		t.Errorf("len(Text) = %d, want %d", len(page.Text), DefaultBytes)
	}
	if page.Offset != 0 || page.End() != DefaultBytes {
		t.Errorf("window = [%d,%d), want [0,%d)", page.Offset, page.End(), DefaultBytes)
	}
	if !page.Truncated() || page.Next() != DefaultBytes {
		t.Errorf("Truncated = %v, Next = %d; want true, %d", page.Truncated(), page.Next(), DefaultBytes)
	}
}

func TestAt_PagesConcatenateToTheOriginal(t *testing.T) {
	report := strings.Repeat("the quick brown fox — ünïcodé ✓ ", 3000)
	var got strings.Builder
	offset := 0
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("paging never reached the end")
		}
		page := At(report, offset, 977) // deliberately not a rune multiple
		got.WriteString(page.Text)
		if !page.Truncated() {
			break
		}
		if page.Next() <= offset {
			t.Fatalf("page did not advance: offset %d, next %d", offset, page.Next())
		}
		offset = page.Next()
	}
	if got.String() != report {
		t.Fatalf("paged bytes differ from the original (got %d bytes, want %d)", got.Len(), len(report))
	}
}

func TestAt_NeverSplitsARune(t *testing.T) {
	// "é" is two bytes; a 3-byte window that starts at 0 would land mid-rune
	// without the snap.
	report := "éééé"         // 8 bytes
	page := At(report, 1, 3) // requested start is inside the first rune
	if !utf8.ValidString(page.Text) {
		t.Fatalf("page %q is not valid UTF-8", page.Text)
	}
	if page.Offset != 0 {
		t.Errorf("Offset = %d, want 0 (snapped down)", page.Offset)
	}
	if page.Text != "é" {
		t.Errorf("Text = %q, want %q", page.Text, "é")
	}
	tail := At(report, page.Next(), 3)
	if !utf8.ValidString(tail.Text) {
		t.Fatalf("tail page %q is not valid UTF-8", tail.Text)
	}
	if page.Text+tail.Text != report[:len(page.Text+tail.Text)] {
		t.Errorf("pages do not line up: %q + %q", page.Text, tail.Text)
	}
}

func TestAt_ClampsRatherThanRefusing(t *testing.T) {
	report := "0123456789"
	cases := []struct {
		name       string
		offset     int
		limit      int
		wantText   string
		wantOffset int
		wantLimit  int
	}{
		{"negative offset is the head", -5, 4, "0123", 0, 4},
		{"offset past the end is the empty tail", 50, 4, "", 10, 4},
		{"offset exactly at the end is the empty tail", 10, 4, "", 10, 4},
		{"zero limit falls back to the default", 0, 0, report, 0, DefaultBytes},
		{"negative limit falls back to the default", 0, -1, report, 0, DefaultBytes},
		{"oversized limit falls back to the default", 0, MaxBytes + 1, report, 0, DefaultBytes},
		{"limit at the max is honoured", 0, MaxBytes, report, 0, MaxBytes},
		{"the last page is a short read", 8, 4, "89", 8, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := At(report, tc.offset, tc.limit)
			if page.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", page.Text, tc.wantText)
			}
			if page.Offset != tc.wantOffset {
				t.Errorf("Offset = %d, want %d", page.Offset, tc.wantOffset)
			}
			if page.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", page.Limit, tc.wantLimit)
			}
			if page.Total != len(report) {
				t.Errorf("Total = %d, want %d", page.Total, len(report))
			}
		})
	}
}

func TestAt_EmptyReport(t *testing.T) {
	page := At("", 0, DefaultBytes)
	if page.Text != "" || page.Total != 0 || page.Truncated() {
		t.Fatalf("page = %+v, want an empty, complete page with Total 0", page)
	}
	if got := Marker(page, "route"); got != "" {
		t.Errorf("Marker on an untruncated page = %q, want empty", got)
	}
}

func TestMarker_NamesTheWindowAndTheRoute(t *testing.T) {
	report := strings.Repeat("x", 9000)
	page := Head(report, DefaultBytes)
	got := Marker(page, "retrieve more with proposal show")
	want := "\n...[report truncated: showing bytes 0-4000 of 9000; retrieve more with proposal show]"
	if got != want {
		t.Errorf("Marker = %q, want %q", got, want)
	}
}

func TestMarker_EmptyRouteSaysNothingIsRetrievable(t *testing.T) {
	page := Head(strings.Repeat("x", 9000), DefaultBytes)
	got := Marker(page, "")
	if !strings.Contains(got, "not retained") {
		t.Fatalf("Marker with no route = %q, want it to say the rest was not retained", got)
	}
}

// TestAt_NarrowLimitStillAdvances pins the livelock fix (PR #419 review
// finding 1): report_limit may legally be 1, and a limit narrower than the
// rune at start used to snap both edges to the same boundary — an empty page
// whose Next() equals its Offset, so a caller looping "request next offset
// while there is one" spun forever. A page must now always advance.
func TestAt_NarrowLimitStillAdvances(t *testing.T) {
	report := "a🀄b" // a=1 byte, 🀄=4 bytes, b=1 byte
	offset := 0
	seen := 0
	for {
		page := At(report, offset, 1) // narrower than every multi-byte rune
		if page.Next() <= page.Offset {
			t.Fatalf("page at offset %d did not advance: %+v", offset, page)
		}
		if !utf8.ValidString(page.Text) {
			t.Fatalf("page at offset %d split a rune: %q", offset, page.Text)
		}
		offset = page.Next()
		seen++
		if !page.Truncated() {
			break
		}
		if seen > len(report) {
			t.Fatal("paging did not terminate")
		}
	}
	// The whole report must be covered: rebuild it from the pages.
	var got strings.Builder
	for off := 0; off < len(report); {
		page := At(report, off, 1)
		got.WriteString(page.Text)
		off = page.Next()
	}
	if got.String() != report {
		t.Errorf("narrow-limit pages rebuilt %q, want %q", got.String(), report)
	}
}
