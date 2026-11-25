package substrate

import "unicode/utf8"

// ToolResultMaxBytes caps the success detail a single tool call returns. The
// agent loop applies a second, larger cap with spillover; this one exists so a
// tool never hands the loop more than a bounded payload.
const ToolResultMaxBytes = 8192

// TruncationMarker is the marker yaah appends when it caps a payload: capped
// tool output, a bounded proposal diff, or a dispatch note. The marker itself
// makes NO storage claim — it says only that text was cut. Which of the two
// families it belongs to is decided at the call site (yaah-vw1v):
//
//   - TruncationMarker, for a bounded VIEW whose full text is still on disk —
//     the read tool's offset/limit window, a proposal diff bounded for an
//     envelope that the sealed proposal still holds.
//   - DroppedMarker, for a cap that truly DISCARDS bytes nothing stores — the
//     folded body of a failed run, a dispatch reason, a tool result with no
//     spool behind it.
//
// TruncateAt is accordingly for views only; a discard site calls
// TruncateAtDropped.
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
// It makes no storage claim of its own: the caller decides whether the rest of
// the payload is still on disk (a view) or gone (use TruncateAtDropped).
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
