// Package conflictapi holds the conflict-evidence vocabulary shared by the
// built-in tools that record writes and the agent loop that renders the
// resulting report. Keeping it neutral lets the engine depend on the data
// shape without importing internal/tools.
package conflictapi

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// ConflictLaneShared is a dispatch that writes the session workspace
	// directly. Two labels on one path are a real clobber.
	ConflictLaneShared = "shared"
	// ConflictLaneRetained is an isolated proposal/report world. Two labels on
	// one path are only a speculative overlap; the proposal registry is
	// authoritative for settlement.
	ConflictLaneRetained = "retained"

	ConflictKindShared             = "shared_write_conflict"
	ConflictKindRetained           = "retained_speculative_overlap"
	ConflictKindUnverifiedShared   = "shared_unverified_change"
	ConflictKindUnverifiedRetained = "retained_unverified_change"
)

// ConflictDispatch identifies one delegate call for conflict evidence.
type ConflictDispatch struct {
	ID    string
	Label string
	Lane  string
}

// FileRecord is one observed write or mutation.
type FileRecord struct {
	DispatchID    string
	SubAgentLabel string
	Lane          string
	FilePath      string
	Key           string
	ToolName      string

	BeforeExists bool
	AfterExists  bool
	BeforeDigest string
	AfterDigest  string
	BeforeMode   string
	AfterMode    string
	Known        bool

	Unknown bool
	Detail  string
}

// ConflictEntry is one typed conflict, grouped by path and lane.
type ConflictEntry struct {
	Path    string
	Kind    string
	Lane    string
	Records []FileRecord
}

// ConflictReport is the typed result of one detection pass.
type ConflictReport struct {
	Entries []ConflictEntry
	Unknown []FileRecord
}

// FileCount is the number of reported paths plus unknown mutations.
func (r *ConflictReport) FileCount() int {
	if r == nil {
		return 0
	}
	return len(r.Entries) + len(r.Unknown)
}

// Render renders the typed report for the parent conversation.
func (r *ConflictReport) Render() string {
	if r == nil || (len(r.Entries) == 0 && len(r.Unknown) == 0) {
		return ""
	}
	var b strings.Builder
	hasShared := false

	renderLane := func(lane, kind, header string) {
		var entries []ConflictEntry
		for _, entry := range r.Entries {
			if entry.Lane == lane && entry.Kind == kind {
				entries = append(entries, entry)
			}
		}
		if len(entries) == 0 {
			return
		}
		if lane == ConflictLaneShared {
			hasShared = true
		}
		b.WriteString(header)
		b.WriteString("\n\n")
		for _, entry := range entries {
			fmt.Fprintf(&b, "File: %s\n", entry.Path)
			for _, rec := range entry.Records {
				fmt.Fprintf(&b, "  - %s: %s\n", rec.SubAgentLabel, rec.ToolName)
			}
			b.WriteString("\n")
		}
	}

	renderUnknown := func(lane, header string) {
		counts := make(map[string]int)
		details := make(map[string]string)
		for _, rec := range r.Unknown {
			laneOf := rec.Lane
			if laneOf == "" {
				laneOf = ConflictLaneShared
			}
			if laneOf != lane {
				continue
			}
			key := rec.SubAgentLabel + "\x00" + rec.ToolName
			counts[key]++
			if details[key] == "" {
				details[key] = rec.Detail
			}
		}
		if len(counts) == 0 {
			return
		}
		keys := make([]string, 0, len(counts))
		for key := range counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteString(header)
		b.WriteString("\n\n")
		for _, key := range keys {
			label, tool, _ := strings.Cut(key, "\x00")
			fmt.Fprintf(&b, "  - %s: %s", label, tool)
			if n := counts[key]; n > 1 {
				fmt.Fprintf(&b, " (%d calls)", n)
			}
			if details[key] != "" {
				fmt.Fprintf(&b, " (%s)", details[key])
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	renderLane(ConflictLaneShared, ConflictKindShared, "CONFLICT: Parallel workers modified the same file(s).")
	renderLane(ConflictLaneShared, ConflictKindUnverifiedShared, "UNVERIFIED CHANGES: Parallel workers changed the same path but the content could not be inspected.")
	renderLane(ConflictLaneRetained, ConflictKindRetained, "SPECULATIVE OVERLAP: Parallel workers in isolated worlds changed the same file(s). Nothing has landed; proposal list is authoritative.")
	renderLane(ConflictLaneRetained, ConflictKindUnverifiedRetained, "SPECULATIVE UNVERIFIED CHANGES: Isolated workers changed the same path but the content could not be inspected; proposal list is authoritative.")
	renderUnknown(ConflictLaneShared, "UNKNOWN MUTATIONS: Shell commands ran and may have changed files not tracked by structured write tools.")
	renderUnknown(ConflictLaneRetained, "SPECULATIVE UNKNOWN MUTATIONS: Shell commands ran in isolated worlds and may have changed files there.")

	if hasShared {
		b.WriteString("Inspect each file with the read tool, decide on the correct version,")
		b.WriteString(" and resolve with targeted edits or by re-dispatching a single worker.")
	}
	return b.String()
}
