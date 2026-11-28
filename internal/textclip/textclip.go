// Package textclip owns "cut a string on a UTF-8 boundary" for yaah. The
// byte/rune walk used to be copied into individual callers across the agent
// pipeline, providers, tools, memory, ACP and the CLI; they differ only in
// budget unit and marker policy, so the dangerous cut lives here and each
// caller composes its own marker.
package textclip

import "unicode/utf8"

// Bytes keeps at most max bytes from the start of s, backing off to a rune
// boundary. truncated reports whether a cut happened. max <= 0 means no cap.
func Bytes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Runes keeps at most max runes from the start of s. truncated reports whether
// a cut happened. max <= 0 means no cap.
func Runes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// TailBytes keeps at most the last max bytes of s, advancing to a rune
// boundary. truncated reports whether a cut happened. max <= 0 means no cap.
func TailBytes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:], true
}

// Head caps s at max bytes and appends marker when it cuts. The marker is
// outside the budget, so the result can be longer than max. max <= 0 means
// no cap.
func Head(s string, max int, marker string) string {
	kept, cut := Bytes(s, max)
	if cut {
		return kept + marker
	}
	return kept
}

// HeadWithin caps s at max bytes including marker when it cuts. When the
// budget cannot hold the marker, no marker is emitted and the result is empty
// rather than exceeding the cap.
func HeadWithin(s string, max int, marker string) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(marker)
	if budget <= 0 {
		return ""
	}
	kept, cut := Bytes(s, budget)
	if !cut {
		return s
	}
	return kept + marker
}

// Tail caps s at max bytes from the end and prefixes marker when it cuts.
// The marker is outside the budget; max <= 0 means no cap.
func Tail(s string, max int, marker string) string {
	kept, cut := TailBytes(s, max)
	if cut {
		return marker + kept
	}
	return kept
}

// HeadRunes caps s at max runes and appends marker when it cuts. The marker is
// outside the budget; max <= 0 means no cap.
func HeadRunes(s string, max int, marker string) string {
	kept, cut := Runes(s, max)
	if cut {
		return kept + marker
	}
	return kept
}

// HeadRunesWithin caps s at max runes including marker when it cuts. When the
// budget cannot hold the marker, no marker is emitted.
func HeadRunesWithin(s string, max int, marker string) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	budget := max - utf8.RuneCountInString(marker)
	if budget <= 0 {
		return ""
	}
	kept, cut := Runes(s, budget)
	if !cut {
		return s
	}
	return kept + marker
}
