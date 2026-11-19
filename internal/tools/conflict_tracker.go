package tools

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/goatherdai/yaah/internal/conflictapi"
)

const (
	ConflictLaneShared             = conflictapi.ConflictLaneShared
	ConflictLaneRetained           = conflictapi.ConflictLaneRetained
	ConflictKindShared             = conflictapi.ConflictKindShared
	ConflictKindRetained           = conflictapi.ConflictKindRetained
	ConflictKindUnverifiedShared   = conflictapi.ConflictKindUnverifiedShared
	ConflictKindUnverifiedRetained = conflictapi.ConflictKindUnverifiedRetained

	// conflictTrackerMaxHashBytes bounds the file read used to prove a write
	// changed content. A larger file is recorded as unknown rather than read
	// into memory.
	conflictTrackerMaxHashBytes = 32 << 20
)

// FileRecord, ConflictEntry and ConflictReport live in conflictapi so the
// agent loop can name the report shape without importing this package.
type FileRecord = conflictapi.FileRecord
type ConflictEntry = conflictapi.ConflictEntry
type ConflictReport = conflictapi.ConflictReport

// ConflictTracker records successful sub-agent writes and shell mutations for
// the parent loop. It deliberately records after execution: a failed write is
// not evidence of a change.
type ConflictTracker struct {
	mu      sync.Mutex
	records []FileRecord
}

// RecordChange adds one evidence-backed change. It is called after a tool
// succeeds; no-op writes are filtered by the caller.
func (ct *ConflictTracker) RecordChange(rec FileRecord) {
	if rec.FilePath == "" {
		return
	}
	rec.FilePath = normalizeTrackedPath(rec.FilePath)
	if rec.FilePath == "" {
		return
	}
	rec.Key = pathKey(rec.FilePath)
	if rec.DispatchID == "" {
		rec.DispatchID = rec.SubAgentLabel
	}
	if rec.Lane == "" {
		rec.Lane = ConflictLaneShared
	}
	ct.mu.Lock()
	ct.records = append(ct.records, rec)
	ct.mu.Unlock()
}

// RecordUnknown records a mutation whose changed paths the tracker could not
// observe: a shell command, or a recursive rmdir that removed a tree. The
// tracker cannot name the paths, so this is an explicit unknown rather than a
// false all-clear.
func (ct *ConflictTracker) RecordUnknown(dispatch ConflictDispatch, toolName, detail string) {
	rec := FileRecord{
		DispatchID:    dispatch.ID,
		SubAgentLabel: dispatch.Label,
		Lane:          dispatch.Lane,
		ToolName:      toolName,
		Unknown:       true,
		Detail:        detail,
	}
	if rec.DispatchID == "" {
		rec.DispatchID = rec.SubAgentLabel
	}
	if rec.Lane == "" {
		rec.Lane = ConflictLaneShared
	}
	ct.mu.Lock()
	ct.records = append(ct.records, rec)
	ct.mu.Unlock()
}

// DetectAndReset preserves the prose-facing API. The returned report is a view
// over the typed records; callers that need structure use DetectAndResetReport.
func (ct *ConflictTracker) DetectAndReset() string {
	report := ct.DetectAndResetReport()
	if report == nil {
		return ""
	}
	return report.Render()
}

// DetectAndResetReport consumes the current records and classifies them.
func (ct *ConflictTracker) DetectAndResetReport() *ConflictReport {
	ct.mu.Lock()
	records := ct.records
	ct.records = nil
	ct.mu.Unlock()

	if len(records) == 0 {
		return nil
	}

	type pathLane struct {
		key  string
		lane string
	}
	byPath := make(map[pathLane][]FileRecord)
	report := &ConflictReport{}
	for _, rec := range records {
		if rec.Unknown || rec.FilePath == "" {
			report.Unknown = append(report.Unknown, rec)
			continue
		}
		lane := rec.Lane
		if lane == "" {
			lane = ConflictLaneShared
		}
		key := rec.Key
		if key == "" {
			key = pathKey(rec.FilePath)
		}
		group := pathLane{key: key, lane: lane}
		byPath[group] = append(byPath[group], rec)
	}

	for group, recs := range byPath {
		dispatches := make(map[string][]FileRecord)
		for _, rec := range recs {
			id := rec.DispatchID
			if id == "" {
				id = rec.SubAgentLabel
			}
			dispatches[id] = append(dispatches[id], rec)
		}
		if len(dispatches) < 2 {
			continue
		}
		ids := make([]string, 0, len(dispatches))
		for id := range dispatches {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		identical := true
		for i := 0; i < len(ids) && identical; i++ {
			for j := i + 1; j < len(ids); j++ {
				if !conflictGroupsIdentical(dispatches[ids[i]], dispatches[ids[j]]) {
					identical = false
					break
				}
			}
		}
		if identical {
			continue
		}

		allKnown := true
		for _, id := range ids {
			if known, _, _, _ := conflictGroupState(dispatches[id]); !known {
				allKnown = false
				break
			}
		}
		kind := ConflictKindShared
		if !allKnown {
			kind = ConflictKindUnverifiedShared
		}
		if group.lane == ConflictLaneRetained {
			kind = ConflictKindRetained
			if !allKnown {
				kind = ConflictKindUnverifiedRetained
			}
		}
		entry := ConflictEntry{
			Path:    recs[0].FilePath,
			Kind:    kind,
			Lane:    group.lane,
			Records: recs,
		}
		report.Entries = append(report.Entries, entry)
	}

	sort.Slice(report.Entries, func(i, j int) bool {
		if report.Entries[i].Lane != report.Entries[j].Lane {
			return report.Entries[i].Lane < report.Entries[j].Lane
		}
		return report.Entries[i].Path < report.Entries[j].Path
	})
	sort.Slice(report.Unknown, func(i, j int) bool {
		if report.Unknown[i].SubAgentLabel != report.Unknown[j].SubAgentLabel {
			return report.Unknown[i].SubAgentLabel < report.Unknown[j].SubAgentLabel
		}
		return report.Unknown[i].ToolName < report.Unknown[j].ToolName
	})

	if len(report.Entries) == 0 && len(report.Unknown) == 0 {
		return nil
	}
	return report
}

// conflictGroupsIdentical reports whether two dispatch groups observed the same
// resulting state. Unknown evidence is never treated as identical: the tracker
// must not turn an unobserved write into a false all-clear.
func conflictGroupsIdentical(a, b []FileRecord) bool {
	aKnown, aExists, aDigest, aMode := conflictGroupState(a)
	bKnown, bExists, bDigest, bMode := conflictGroupState(b)
	if !aKnown || !bKnown {
		return false
	}
	if aExists != bExists {
		return false
	}
	if !aExists {
		return true
	}
	return aDigest == bDigest && aMode == bMode
}

func conflictGroupState(records []FileRecord) (known, exists bool, digest, mode string) {
	if len(records) == 0 {
		return false, false, "", ""
	}
	last := records[len(records)-1]
	known = last.Known
	exists = last.AfterExists
	digest = last.AfterDigest
	mode = last.AfterMode
	for _, rec := range records {
		if !rec.Known {
			known = false
			break
		}
	}
	return known, exists, digest, mode
}

func normalizeTrackedPath(p string) string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	return path.Clean(p)
}
