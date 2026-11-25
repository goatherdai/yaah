package substrate

import "unicode/utf8"

// ToolResultMaxBytes caps the success detail a single tool call returns. The
// agent loop applies a second, larger cap with spillover; this one exists so a
// tool never hands the loop more than a bounded payload.
const ToolResultMaxBytes = 8192

// TruncationMarker is the marker yaah appends when it caps a payload for
// display: capped tool output, a bounded proposal diff, or a dispatch note.
// Since yaah-vw1v it means a bounded VIEW — the full text is still on disk
// and one offset/limit read away. A cap that truly DISCARDS bytes uses
// DroppedMarker instead.
const TruncationMarker = "\n...[truncated]..."

// DroppedMarker is appended when a cap truly DISCARDS bytes that nothing
// stores (yaah-vw1v): no spool, no on-disk copy — the notice says so outright
// and names the recovery route (re-request a narrower range) rather than
// inviting a hunt for a file nobody wrote. It lives beside TruncationMarker
// because the tools layer and the dispatch layer both fold over-long text (a
// tool result, a failed run's partial report) and the two marker families
// must not drift.
const DroppedMarker = "\n...[truncated — the remainder was dropped and is not stored; " +
	"re-request a narrower range (offset/limit, head, tail) to read it]..."

// TruncateAt caps s at max bytes on a UTF-8 rune boundary and appends
// TruncationMarker when it cuts. max <= 0 means no cap.
//
// It lives with the workspace substrate so a dispatch note and a tool result
// are capped the same way; the callers differ, the convention does not.
func TruncateAt(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncationMarker
}

// TruncateAtDropped caps s at max bytes on a UTF-8 rune boundary and appends
// DroppedMarker when it cuts: the remainder is discarded, not viewable
// elsewhere. max <= 0 means no cap.
func TruncateAtDropped(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + DroppedMarker
}

// TailTruncationMarker prefixes text kept from the END of an over-long result.
const TailTruncationMarker = "...[earlier text truncated]..."

// KeepTail caps s at max bytes on a UTF-8 rune boundary, keeping the LAST
// max bytes and prefixing TailTruncationMarker when it cuts. Teardown notes
// end with the worktree path they are reporting, so a note that must fit a
// budget keeps its tail, not its head.
func KeepTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return TailTruncationMarker + s[cut:]
}
