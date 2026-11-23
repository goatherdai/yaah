package tools

// Post-commit write evidence: the recording wrapper's after-image for a
// self-guarding tool (yaah-1615).
//
// RecordingTool records a write by snapshotting a path, running the tool, and
// snapshotting it again. For a tool that takes the per-path lock inside its own
// Execute (selfGuardingTool: write, edit, patch, json_query, append, move,
// copy, replace, bulk_files) the wrapper cannot hold that lock across the call
// — it is refcounted but not reentrant per goroutine, so the wrapper's
// acquisition would deadlock the wrapped tool's own. The after-image was
// therefore taken AFTER the tool had already released its critical section, and
// a concurrent dispatch's write landing in that window was attributed to this
// dispatch: the bytes were right, the diagnosis was wrong.
//
// The fix moves the after-image capture inside the critical section. The tool
// publishes it through a sink the wrapper installs in the context, and the
// wrapper prefers a published image over taking its own read. Two publish
// points exist:
//
//   - withFileWrite calls publishFileWriteEvidence immediately after its
//     successful write, while its per-path lock is still held (see its
//     onCommit hook in fileguard.go). Every tool whose mutation goes through
//     the guard — write, edit, patch, json_query — gets covered with no
//     per-tool plumbing.
//   - tools whose mutation does not go through withFileWrite call
//     publishPathSnapshot from inside their own critical section: append (the
//     substrate append primitive) and move/copy (the substrate rename/copy).
//
// A published image is keyed by the same normalized path the per-path lock
// uses, and the wrapper only consults it for the path it is recording, so a
// transfer's other end — or an unrelated path a tool happens to touch — cannot
// be mistaken for this dispatch's change. Nothing here allocates or reads when
// no sink is installed, so an orchestrator-lane call (no RecordingTool in the
// path) pays nothing.

import (
	"context"
	"sync"
)

const writeEvidenceKey contextKey = "yaah-write-evidence"

// writeEvidenceSink holds the post-mutation images one self-guarding tool
// published from inside its own critical section. It is per dispatch call, so
// two concurrent dispatches never share one.
type writeEvidenceSink struct {
	mu        sync.Mutex
	snapshots map[string]conflictSnapshot
}

// withWriteEvidence installs a sink for the duration of one wrapped Execute.
// The returned context is the one that must be handed to the wrapped tool: the
// sink reaches the tool only through it.
func withWriteEvidence(ctx context.Context) (context.Context, *writeEvidenceSink) {
	sink := &writeEvidenceSink{snapshots: make(map[string]conflictSnapshot, 1)}
	return context.WithValue(ctx, writeEvidenceKey, sink), sink
}

// writeEvidenceFrom returns the sink installed in ctx, or nil when the call
// path carries no recorder.
func writeEvidenceFrom(ctx context.Context) *writeEvidenceSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(writeEvidenceKey).(*writeEvidenceSink)
	return sink
}

// put records one path's post-mutation image.
func (s *writeEvidenceSink) put(abs string, snap conflictSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshots[pathKey(normalizeTrackedPath(abs))] = snap
	s.mu.Unlock()
}

// get returns the image the tool published for abs, if any. A nil sink
// reports nothing, which is what makes every caller below a no-op on a lane
// with no recorder.
func (s *writeEvidenceSink) get(abs string) (conflictSnapshot, bool) {
	if s == nil {
		return conflictSnapshot{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[pathKey(normalizeTrackedPath(abs))]
	return snap, ok
}

// publishFileWriteEvidence is withFileWrite's onCommit hook: it runs while the
// per-path lock for abs is still held, immediately after the successful write,
// so the image it publishes is this dispatch's own post-write state and can
// never be a concurrent dispatch's later write.
func publishFileWriteEvidence(ctx context.Context, ws Workspace, abs string, receipt FileWriteReceipt) {
	sink := writeEvidenceFrom(ctx)
	if sink == nil {
		return
	}
	sink.put(abs, snapshotFromWriteReceipt(ctx, ws, abs, receipt))
}

// publishPathSnapshot captures abs and publishes it as this dispatch's
// post-mutation image. It is the publish point for a tool whose mutation does
// not go through withFileWrite, and it must be called while the caller's
// per-path lock for abs is held — that is what makes the capture its own
// image rather than a concurrent writer's.
func publishPathSnapshot(ctx context.Context, ws Workspace, abs string) {
	sink := writeEvidenceFrom(ctx)
	if sink == nil {
		return
	}
	sink.put(abs, captureConflictSnapshot(ctx, ws, abs))
}

// snapshotFromWriteReceipt builds the post-write image from the write receipt
// plus one Lstat of the path: the digest and size come from the bytes the guard
// just wrote, so the file is never re-read, while the permission bits come from
// the file itself — the written mode is not always the one the guard passed
// (the local lane preserves an existing file's mode, and a create is subject to
// the platform's rules), and a mode that disagreed with every later snapshot
// would turn an identical write into false evidence.
func snapshotFromWriteReceipt(ctx context.Context, ws Workspace, abs string, receipt FileWriteReceipt) conflictSnapshot {
	info, err := ws.Lstat(ctx, abs)
	if err != nil {
		// Unobservable existence is unknown evidence, matching
		// captureConflictSnapshot's error branch.
		return conflictSnapshot{}
	}
	mode := info.Mode().String()
	// A file over the hash cap keeps the same "content unverified" treatment
	// captureConflictSnapshot gives it; the written bytes are known, but the
	// tracker's contract is not to hold a digest it did not hash under the cap.
	if !info.Mode().IsRegular() || receipt.AfterBytes > conflictTrackerMaxHashBytes {
		return conflictSnapshot{Exists: true, Mode: mode}
	}
	return conflictSnapshot{Exists: true, Known: true, Digest: receipt.Digest, Mode: mode}
}

// snapshotAfter returns abs's current image, preferring one the wrapped tool
// published from inside its own per-path critical section: that image is what
// this dispatch produced, while a fresh read can observe a concurrent dispatch
// that wrote after the tool released its lock.
func snapshotAfter(ctx context.Context, ws Workspace, abs string) conflictSnapshot {
	if snap, ok := writeEvidenceFrom(ctx).get(abs); ok {
		return snap
	}
	return captureConflictSnapshot(ctx, ws, abs)
}
