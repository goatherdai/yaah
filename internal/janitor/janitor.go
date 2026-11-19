// Package janitor classifies dispatch residue — stranded worktrees,
// confinement scratch, and the lifecycle state that decides what is safe to
// reclaim — from three read-only sources: the recovery registry (scope ids,
// recorded worktree paths, adoption state), the proposal lifecycle facts the
// dispatch lane appended to the trace (sealed / settled), and the
// filesystem's reality (what exists, how big, last modified).
//
// It decides nothing on its own: Classify produces findings with a
// SafeToDelete verdict per the decision table below, and the caller (the
// yaah janitor command) reports or reclaims according to its mode.
//
// Decision table (janitor-plan.md §3):
//
//	terminal scope + settled proposal + dir on disk → settled-worktree (safe: teardown was missed; the work landed)
//	sealed, never settled + dir on disk             → sealed-unsettled (report-only: work lives in the sealed delta, but the proposal may still be applied later)
//	no proposal facts + dir on disk                 → unsealed-residue (report-only: may hold the only copy of unmerged work)
//	recorded path gone                              → missing (informational; ages out with session retention)
//	adopted by a live scope                         → protected, never touched
//	quarantined lifecycle                           → never touched
//	.yaah-scratch inside any non-adopted dir        → scratch (safe: confinement redirect, housekeeping writes only)
package janitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	shepherdtrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/dispatch"
)

// Kind names a residue class.
type Kind string

const (
	KindSettledWorktree Kind = "settled-worktree" // proposal settled, teardown missed — the safe set
	KindSealedUnsettled Kind = "sealed-unsettled" // sealed, never settled — report-only
	KindUnsealedResidue Kind = "unsealed-residue" // no lifecycle facts — report-only, may hold unmerged work
	KindScratch         Kind = "scratch"          // .yaah-scratch confinement redirect inside a non-adopted dir
	KindMissing         Kind = "missing"          // recorded path is gone — informational
	KindQuarantined     Kind = "quarantined"      // contradictory lifecycle — never touched
	KindAdopted         Kind = "adopted"          // live scope's worktree — protected
)

// ScratchDirName is the confinement redirect directory the tools layer
// creates inside a worktree (substrate.ConfinementScratchName's contract:
// housekeeping writes land there, and the dispatch lane scrubs it before
// sealing — so outside a sealed-and-settled lifecycle it is, by definition,
// disposable).
const ScratchDirName = ".yaah-scratch"

// Finding is one classified residue entry.
type Finding struct {
	Kind         Kind
	Path         string
	ScopeID      string
	ProposalID   string
	Bytes        int64
	ModTime      time.Time
	SafeToDelete bool
	Reason       string
}

// Report is the scan result: findings plus totals. ReclaimableBytes sums
// findings with SafeToDelete — what `janitor run` would reclaim today.
type Report struct {
	Findings         []Finding
	ReclaimableBytes int64
	TotalBytes       int64
	Scopes           int
	Roots            int
	GeneratedAt      time.Time
}

// lifecycleState is a proposal's recorded trace state.
type lifecycleState int

const (
	lifecycleUnknown lifecycleState = iota
	lifecycleSealed
	lifecycleSettled
)

// lifecycleIndex folds the proposal lifecycle facts into per-scope state.
// Facts may land on the proposal's own scope owner or on a parent owner, so
// the index resolves a scope id three ways: exact owner match, the
// proposal-id prefix form ("scope:proposal:<pid>" matches
// "scope:proposal:<pid>:<idx>"), and a payload proposal-id match.
type lifecycleIndex struct {
	state map[string]lifecycleState
	ids   map[string]string
}

func (l lifecycleIndex) stateFor(scopeID string) lifecycleState {
	// Gather every owner's recorded state for this proposal — the dispatch
	// scope's own owner and any parent the settle verb landed on — and let
	// the later verb win across all of them: settled anywhere settles the
	// scope's view, sealed elsewhere only fills an unknown.
	var states []lifecycleState
	if s, ok := l.state[scopeID]; ok {
		states = append(states, s)
	}
	if pid := l.embeddedID(scopeID); pid != "" {
		if s, ok := l.state[scopeIDForProposal(pid)]; ok {
			states = append(states, s)
		}
	}
	for _, s := range states {
		if s == lifecycleSettled {
			return lifecycleSettled
		}
	}
	for _, s := range states {
		if s == lifecycleSealed {
			return lifecycleSealed
		}
	}
	return lifecycleUnknown
}

func (l lifecycleIndex) idFor(scopeID string) string {
	if id, ok := l.ids[scopeID]; ok {
		return id
	}
	if pid := l.embeddedID(scopeID); pid != "" {
		return pid
	}
	return ""
}

// embeddedID extracts the proposal id embedded in a dispatch scope id
// ("scope:proposal:1795051936611977701:2" → "1795051936611977701"); ""
// for other scope shapes.
func (l lifecycleIndex) embeddedID(scopeID string) string {
	parts := strings.Split(scopeID, ":")
	if len(parts) >= 4 && parts[0] == "scope" && parts[1] == "proposal" {
		return parts[2]
	}
	return ""
}

// Classify joins the recovery registry, the proposal lifecycle facts, and
// the filesystem into a residue report. store is the session trace store
// (either backend); report is recovery's registry scan. Everything is
// read-only.
func Classify(ctx context.Context, store *shepherdtrace.SQLiteTraceStore, report *recovery.Report) (*Report, error) {
	index, err := proposalLifecycle(ctx, store)
	if err != nil {
		return nil, fmt.Errorf("read proposal lifecycle: %w", err)
	}

	out := &Report{
		Scopes:      report.Scopes,
		Roots:       report.Roots,
		GeneratedAt: time.Now(),
	}

	add := func(f Finding) {
		out.Findings = append(out.Findings, f)
		out.TotalBytes += f.Bytes
		if f.SafeToDelete {
			out.ReclaimableBytes += f.Bytes
		}
	}

	// Adopted worktrees are live work: listed for completeness, protected.
	for _, p := range report.Adopted {
		add(Finding{Kind: KindAdopted, Path: p, Reason: "live scope's worktree — never touched"})
	}

	// Orphans: terminal scope, worktree on disk and registered. The
	// proposal lifecycle decides between safe and report-only.
	for _, o := range report.Orphans {
		add(worktreeFinding(index, o.ScopeID, o.Path, o.Size))
	}

	// Unadopted: the scope is not terminal and the path exists but is not a
	// registered worktree of its repo — git operations against it fail, so
	// it cannot serve a live scope; the lifecycle still decides.
	for _, u := range report.Unadopted {
		add(worktreeFinding(index, u.ScopeID, u.Path, dirSize(u.Path)))
	}

	// Missing: recorded path is gone. Nothing to clean; the report entry
	// ages out when the owning session is deleted.
	for _, m := range report.Missing {
		add(Finding{Kind: KindMissing, Path: m.Path, ScopeID: m.ScopeID,
			Reason: "recorded worktree is gone — informational; ages out with the owning session"})
	}

	// Quarantined: the trace's lifecycle records contradict. Never touched.
	for _, q := range report.Quarantined {
		add(Finding{Kind: KindQuarantined, Path: q.Path, Reason: "contradictory trace lifecycle — left on disk"})
	}

	sortFindings(out.Findings)
	return out, nil
}

// worktreeFinding classifies one on-disk worktree by its scope's proposal
// lifecycle.
func worktreeFinding(index lifecycleIndex, scopeID, path string, size int64) Finding {
	state := index.stateFor(scopeID)
	proposalID := index.idFor(scopeID)
	f := Finding{
		Path:       path,
		ScopeID:    scopeID,
		ProposalID: proposalID,
		Bytes:      size,
		ModTime:    modTime(path),
	}
	switch state {
	case lifecycleSettled:
		f.Kind = KindSettledWorktree
		f.SafeToDelete = true
		f.Reason = fmt.Sprintf("proposal %s settled — teardown was missed; the landed delta is the record, the worktree is scratch",
			proposalName(proposalID, scopeID))
	case lifecycleSealed:
		f.Kind = KindSealedUnsettled
		f.Reason = fmt.Sprintf("proposal %s sealed but never settled — the sealed delta preserves the work; settle or discard first",
			proposalName(proposalID, scopeID))
	default:
		f.Kind = KindUnsealedResidue
		f.Reason = "no proposal lifecycle facts — may hold the only copy of unmerged work"
	}
	return f
}

// ScanScratch reports the confinement scratch directories inside the
// non-adopted worktrees of an existing classification. Separate from
// Classify so callers can sweep scratch even when the worktree itself is
// report-only: the scratch is redirected housekeeping by definition, never
// agent work.
func ScanScratch(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind == KindAdopted || f.Kind == KindMissing || f.Kind == KindQuarantined || f.Path == "" {
			continue
		}
		scratch := filepath.Join(f.Path, ScratchDirName)
		fi, err := os.Stat(scratch)
		if err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, Finding{
			Kind:         KindScratch,
			Path:         scratch,
			ScopeID:      f.ScopeID,
			Bytes:        dirSize(scratch),
			ModTime:      fi.ModTime(),
			SafeToDelete: true,
			Reason:       "confinement redirect: housekeeping writes (build caches, temp), scrubbed at seal on the normal path",
		})
	}
	sortFindings(out)
	return out
}

// proposalLifecycle reads the sealed/settled facts once and folds them into
// per-scope lifecycle state: settled wins over sealed (the last verb is the
// state), and each fact's proposal id is captured for reporting.
func proposalLifecycle(ctx context.Context, store *shepherdtrace.SQLiteTraceStore) (lifecycleIndex, error) {
	records, err := shepherdtrace.ScanRecordsBySchema(ctx, store,
		dispatch.SchemaProposalSealed, dispatch.SchemaProposalSettled)
	if err != nil {
		return lifecycleIndex{}, err
	}

	index := lifecycleIndex{
		state: make(map[string]lifecycleState),
		ids:   make(map[string]string),
	}
	record := func(ownerKey string, state lifecycleState, proposalID string) {
		if ownerKey == "" {
			return
		}
		if proposalID != "" {
			index.ids[ownerKey] = proposalID
			index.ids[scopeIDForProposal(proposalID)] = proposalID
		}
		// Settled is the later verb and wins; sealed only fills an unknown.
		if state == lifecycleSettled || index.state[ownerKey] == lifecycleUnknown {
			index.state[ownerKey] = state
		}
		if proposalID != "" {
			if prefix := scopeIDForProposal(proposalID); prefix != "" {
				if state == lifecycleSettled || index.state[prefix] == lifecycleUnknown {
					index.state[prefix] = state
				}
			}
		}
	}

	for _, r := range records {
		id := payloadID(r.Payload)
		switch r.SchemaRef {
		case dispatch.SchemaProposalSealed:
			record(r.OwnerID, lifecycleSealed, id)
		case dispatch.SchemaProposalSettled:
			record(r.OwnerID, lifecycleSettled, id)
		}
	}
	return index, nil
}

// scopeIDForProposal derives the proposal scope id prefix for a proposal id
// ("1795051936611977701" → "scope:proposal:1795051936611977701"); "" for an
// empty id. Full dispatch scope ids carry an ordinal suffix
// ("scope:proposal:<id>:<n>"), which prefix-matches this form.
func scopeIDForProposal(proposalID string) string {
	if proposalID == "" {
		return ""
	}
	return "scope:proposal:" + proposalID
}

func payloadID(payload map[string]any) string {
	if id, ok := payload["proposal_id"].(string); ok {
		return id
	}
	return ""
}

func proposalName(proposalID, scopeID string) string {
	if proposalID != "" {
		return proposalID
	}
	return scopeID
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// dirSize sums a directory tree's file sizes. Errors are skipped: a file
// vanishing mid-walk undercounts, it does not misreport.
func dirSize(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			if info, ierr := entry.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Path < findings[j].Path
	})
}
