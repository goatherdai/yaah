package agent

import (
	"fmt"
	"log/slog"
	"strings"

	agentctx "github.com/goatherdai/yaah/internal/agent/context"
)

// Re-exports for backward compatibility within the agent package (tests
// reference the historical unexported names).
const defaultTruncateMaxLines = agentctx.DefaultTruncateMaxLines

func cleanTruncatedDir(dir string) { agentctx.CleanTruncatedDir(dir) }

// truncateToolResult applies the loop's outer context-safety cap to a
// SUCCESSFUL tool result. Tool-level truncation and the never-truncate-errors
// invariant live in internal/tools/result.go: tools put failure detail first,
// so this cap can only ever cut success detail. Errors never reach this
// function — agent_dispatch delivers them verbatim.
func (l *Loop) truncateToolResult(result string) string {
	maxLines := l.ctxMgr().ToolResultMaxLines
	if maxLines <= 0 {
		maxLines = agentctx.DefaultTruncateMaxLines
	}
	maxBytes := l.ctxMgr().ToolResultMaxBytes
	if maxBytes <= 0 {
		maxBytes = agentctx.DefaultTruncateMaxBytes
	}

	lines := strings.Split(result, "\n")
	lineCount := len(lines)
	byteCount := len(result)

	if lineCount <= maxLines && byteCount <= maxBytes {
		return result
	}

	lineCapped := lineCount > maxLines
	byteCapped := byteCount > maxBytes

	var cutIdx int
	var truncated string
	var truncatedLines int

	lineCutPos := agentctx.FindLineCutBytePos(result, maxLines)
	if lineCapped && (!byteCapped || lineCutPos <= maxBytes) {
		cutIdx = lineCutPos
		truncated = result[:cutIdx]
		truncatedLines = maxLines
	} else {
		cutIdx = maxBytes
		if cutIdx < len(result) {
			lastNL := strings.LastIndexByte(result[:cutIdx], '\n')
			if lastNL > 0 {
				cutIdx = lastNL
			}
		}
		truncated = result[:cutIdx]
		truncatedLines = strings.Count(truncated, "\n") + 1
	}

	// Spill directory is injected via LoopConfig.ToolSpillDir by the
	// composition root; empty disables spilling (finding C4).
	//
	// The spill is the RAW result, byte-for-byte (yaah-vw1v): a programmatic
	// consumer can parse a spooled JSON envelope straight off disk, and a byte
	// comparison against this result succeeds. A single-line blob gets a
	// `.wrapped` PAGER SIBLING instead of being wrapped in place, so the
	// canonical file stays the original bytes while offset/limit paging still
	// has line structure to work with.
	spillDir := l.Config.ToolSpillDir
	raw := []byte(result)
	if fullPath, writeErr := agentctx.WriteSpool(spillDir, "tool", raw); writeErr == nil {
		wrapped, wrapErr := agentctx.SpoolPagerSibling(fullPath, raw, agentctx.SpoolPagerWidth)
		if wrapErr != nil {
			// The canonical spill is intact, so the notice stays truthful (it
			// names no pager copy), but the loss of offset/limit paging would
			// otherwise be invisible: record it instead of discarding the error.
			slog.Debug("spill pager copy failed", "spill", fullPath, "err", wrapErr)
		}
		agentctx.CleanTruncatedDir(spillDir)
		hint := fmt.Sprintf(
			"\n...[output truncated at %d lines / %d bytes — %s. Or spawn a subagent for large analysis.]...",
			truncatedLines, len(result), agentctx.RecoveryClause(fullPath, wrapped),
		)
		return truncated + hint
	}

	// No spill landed, so the cut bytes exist nowhere. Say so: a notice that
	// only says "truncated" invites a search for a file that was never written.
	reason := "no spill directory is configured for this session"
	if spillDir != "" {
		reason = "the spill write failed"
	}
	return truncated + fmt.Sprintf(
		"\n...[truncated at %d lines / %d bytes — %s]...",
		truncatedLines, len(result), agentctx.NoSpoolClause(reason),
	)
}
