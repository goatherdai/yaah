package context

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Spill storage policy: display may page, storage never loses.
//
// Two facts govern every spill file:
//
//  1. The canonical spill file holds the output BYTE-FOR-BYTE. No wrapping,
//     no re-flowing, no trailing-newline fixups: a consumer can hand a spooled
//     JSON envelope straight to encoding/json, and a byte comparison against
//     the original output succeeds. Earlier the spill of a single-line blob
//     was hard-wrapped so the read tool could page it, which made the file a
//     *derived* artifact — every consumer had to reconstruct the original by
//     joining lines, and grep/tail on the spill returned garbage because a
//     wrap can split a token.
//
//  2. A single-line blob gets a `.wrapped` SIBLING, never a wrapped canonical
//     file. Line-based readers (the read tool's offset/limit) cannot page a
//     one-line file: the offset/limit pair selects lines, and one line is the
//     whole blob. The pager copy exists for that reason alone — it is a
//     derived, display-only artifact sitting next to the canonical bytes, so
//     no consumer ever has to guess which one it is holding.

// spoolWriteAttempts bounds the O_EXCL retry loop. Collisions only happen when
// two spills land in the same clock tick, so more than a couple of retries is
// a signal that something else is wrong (a full disk, a permission problem),
// not a reason to spin.
const spoolWriteAttempts = 4

// WriteSpool writes data to a new file under dir and returns its path. The
// bytes are written verbatim: data is the file's content, exactly.
//
// kind names the producer in the filename ("tool", "prune"), keeping the
// established `<id>-<kind>.txt` shape. An existing file is never replaced, so
// a caller can never silently lose an earlier spill.
func WriteSpool(dir, kind string, data []byte) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("no spill directory configured")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < spoolWriteAttempts; attempt++ {
		name := fmt.Sprintf("%d-%s.txt", time.Now().UnixNano(), kind)
		if attempt > 0 {
			name = fmt.Sprintf("%d-%d-%s.txt", time.Now().UnixNano(), attempt, kind)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			lastErr = err
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			os.Remove(path)
			return "", werr
		}
		if cerr != nil {
			os.Remove(path)
			return "", cerr
		}
		return path, nil
	}
	return "", lastErr
}

// SpoolPagerSibling writes a line-wrapped PAGER COPY of a spill next to it and
// returns its path. It returns "" (and no error) when the canonical spill needs
// no pager copy: content that already has line structure is pageable as it
// stands, and a copy of it would be a second, divergent artifact for no gain.
//
// width is the wrap width in bytes. The canonical file is never touched, so
// joining the sibling's lines reconstructs it exactly when it had no newlines
// to begin with, and searching the canonical file never sees a split token.
func SpoolPagerSibling(spillPath string, data []byte, width int) (string, error) {
	if spillPath == "" || countNewlines(data) > 1 || len(data) <= width {
		return "", nil
	}
	sibling := spillPath + ".wrapped"
	if err := os.WriteFile(sibling, []byte(WrapForPaging(string(data), width)), 0o644); err != nil {
		return "", err
	}
	return sibling, nil
}

// countNewlines counts '\n' bytes without converting data to a string.
func countNewlines(data []byte) int {
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

// SpoolPagerWidth is the wrap width for a spill's pager copy. 200 bytes keeps
// a pager read's line windows small enough to stay under the tool-result cap
// (a 200-byte width means ~40 lines of a pager copy fit in 8 KiB), so
// offset/limit can actually walk a single-line blob.
const SpoolPagerWidth = 200

// RecoveryClause is the sentence a truncation notice carries to name its
// read path: the absolute spill path, the tool that pages it, and the fact
// that the file is the raw output rather than a reformatted copy. wrapped, when
// non-empty, names the pager sibling.
//
// The `full output at <path>. To read portions` shape is load-bearing:
// internal/agent/pipeline recovers the path from trace and evidence payloads
// with `full output at (.+?)\. To read portions` (traceSpillPattern) and
// `full output at \S+` (spillPathPattern). Change the shape and those two
// consumers silently stop seeing the spill.
func RecoveryClause(path, wrapped string) string {
	clause := "full output at " + path +
		". To read portions, use the read tool with offset/limit against that file: " +
		"it holds the raw output byte-for-byte, with no line wrapping"
	if wrapped != "" {
		clause += "; a line-wrapped copy for paging is at " + wrapped
	}
	return clause
}

// NoSpoolClause is the honest alternative to RecoveryClause when a truncation
// dropped bytes that nothing else holds. A notice that omits the middle of a
// output without naming a copy of it invites a reader to hunt for a spill that
// does not exist, so every pruning site states which case it is.
func NoSpoolClause(reason string) string {
	if reason == "" {
		reason = "no spill directory is configured"
	}
	return "the dropped bytes are not stored anywhere (" + reason + ")"
}
