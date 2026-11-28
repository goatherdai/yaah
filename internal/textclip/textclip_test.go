package textclip

import (
	"testing"
	"unicode/utf8"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		max       int
		want      string
		wantTrunc bool
	}{
		{"empty", "", 4, "", false},
		{"shorter", "hi", 10, "hi", false},
		{"exact", "hello", 5, "hello", false},
		{"ascii", "hello world", 5, "hello", true},
		{"two-byte straddled", "héllo", 2, "h", true},
		{"two-byte at start", "héllo", 1, "h", true},
		{"three-byte straddled", "日本語", 4, "日", true},
		{"three-byte exact", "日本語", 3, "日", true},
		{"zero disables", "日本語", 0, "日本語", false},
		{"negative disables", "日本語", -1, "日本語", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := Bytes(tc.in, tc.max)
			if got != tc.want || trunc != tc.wantTrunc {
				t.Fatalf("Bytes(%q,%d) = (%q,%v), want (%q,%v)", tc.in, tc.max, got, trunc, tc.want, tc.wantTrunc)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("output %q is not valid UTF-8", got)
			}
		})
	}
}

func TestRunes(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		max       int
		want      string
		wantTrunc bool
	}{
		{"empty", "", 4, "", false},
		{"exact", "héllo", 5, "héllo", false},
		{"cut inside", "héllo", 4, "héll", true},
		{"multibyte", "日本語", 2, "日本", true},
		{"one", "日本語", 1, "日", true},
		{"zero disables", "日本語", 0, "日本語", false},
		{"longer than runes", "héllo", 9, "héllo", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := Runes(tc.in, tc.max)
			if got != tc.want || trunc != tc.wantTrunc {
				t.Fatalf("Runes(%q,%d) = (%q,%v), want (%q,%v)", tc.in, tc.max, got, trunc, tc.want, tc.wantTrunc)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("output %q is not valid UTF-8", got)
			}
		})
	}
}

func TestTailBytes(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		max       int
		want      string
		wantTrunc bool
	}{
		{"empty", "", 4, "", false},
		{"exact", "hello", 5, "hello", false},
		{"ascii", "hello world", 5, "world", true},
		{"two-byte", "héllo", 2, "lo", true},
		{"three-byte straddled", "日本語", 4, "語", true},
		{"three-byte too small", "日本語", 2, "", true},
		{"zero disables", "日本語", 0, "日本語", false},
		{"negative disables", "日本語", -1, "日本語", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, trunc := TailBytes(tc.in, tc.max)
			if got != tc.want || trunc != tc.wantTrunc {
				t.Fatalf("TailBytes(%q,%d) = (%q,%v), want (%q,%v)", tc.in, tc.max, got, trunc, tc.want, tc.wantTrunc)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("output %q is not valid UTF-8", got)
			}
		})
	}
}

func TestHead(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		mark string
		want string
	}{
		{"short", "hi", 5, "...", "hi"},
		{"cut", "hello world", 5, "...", "hello..."},
		{"multibyte", "héllo", 1, "…", "h…"},
		{"disabled", "hello", 0, "...", "hello"},
		{"empty marker", "hello world", 5, "", "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Head(tc.in, tc.max, tc.mark); got != tc.want {
				t.Fatalf("Head(%q,%d,%q) = %q, want %q", tc.in, tc.max, tc.mark, got, tc.want)
			}
		})
	}
}

func TestHeadWithin(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		mark string
		want string
	}{
		{"short", "hello", 6, "...", "hello"},
		{"reserves marker", "hello world", 8, "...", "hello..."},
		{"budget equals marker", "hello world", 3, "...", ""},
		{"budget below marker", "hello world", 2, "...", ""},
		{"multibyte", "日本語", 6, "…", "日…"},
		{"disabled", "hello world", 0, "...", "hello world"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HeadWithin(tc.in, tc.max, tc.mark)
			if got != tc.want {
				t.Fatalf("HeadWithin(%q,%d,%q) = %q, want %q", tc.in, tc.max, tc.mark, got, tc.want)
			}
			if len(got) > tc.max && tc.max > 0 {
				t.Fatalf("output %q exceeds byte budget %d", got, tc.max)
			}
		})
	}
}

func TestTail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		mark string
		want string
	}{
		{"short", "hi", 5, "[earlier] ", "hi"},
		{"cut", "hello world", 5, "[earlier] ", "[earlier] world"},
		{"multibyte", "日本語", 4, "…", "…語"},
		{"disabled", "hello", 0, "…", "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Tail(tc.in, tc.max, tc.mark); got != tc.want {
				t.Fatalf("Tail(%q,%d,%q) = %q, want %q", tc.in, tc.max, tc.mark, got, tc.want)
			}
		})
	}
}

func TestHeadRunes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		mark string
		want string
	}{
		{"short", "hi", 5, "…", "hi"},
		{"cut", "héllo", 4, "…", "héll…"},
		{"disabled", "héllo", 0, "…", "héllo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HeadRunes(tc.in, tc.max, tc.mark); got != tc.want {
				t.Fatalf("HeadRunes(%q,%d,%q) = %q, want %q", tc.in, tc.max, tc.mark, got, tc.want)
			}
		})
	}
}

func TestHeadRunesWithin(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		mark string
		want string
	}{
		{"byte longer but runes fit", "héllo", 5, "…", "héllo"},
		{"reserves marker", "héllo world", 6, "…", "héllo…"},
		{"budget equals marker", "abc", 1, "…", ""},
		{"disabled", "héllo", 0, "…", "héllo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HeadRunesWithin(tc.in, tc.max, tc.mark)
			if got != tc.want {
				t.Fatalf("HeadRunesWithin(%q,%d,%q) = %q, want %q", tc.in, tc.max, tc.mark, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > tc.max && tc.max > 0 {
				t.Fatalf("output %q has %d runes, budget is %d", got, n, tc.max)
			}
		})
	}
}
