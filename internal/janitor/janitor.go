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
//	active scope + any proposal state              → unsealed-residue (report-only: the scope is still usable — a settle fact is appended before teardown, so a failed teardown or refused adoption leaves active scopes that must not be reclaimed)
//	sealed, never settled + dir on disk            → sealed-unsettled (report-only: work lives in the sealed delta, but the proposal may still be applied later)
//	no proposal facts + dir on disk                → unsealed-residue (report-only: may hold the only copy of unmerged work)
//	recorded path gone                             → missing (informational; ages out with session retention)
//	adopted by a live scope                        → protected, never touched
//	quarantined lifecycle                          → never touched
//	.yaah-scratch inside a settled worktree        → scratch (safe: confinement redirect inside worktree the janitor already owns)
//
// Only settled worktrees whose directory is verifiably the dispatch-created
// worktree (path lives under the configured retained-worktree root) enter
// the safe set: recovery includes any existing path for a terminal scope in
// Orphans, and an unrelated occupant of that path must not be mislabeled.
package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	KindSettledWorktree Kind = "settled-worktree" // terminal scope, proposal settled — the safe set
	KindSealedUnsettled Kind = "sealed-unsettled" // sealed, never settled — report-only
	KindUnsealedResidue Kind = "unsealed-residue" // active scope, or no lifecycle facts — report-only
	KindScratch         Kind = "scratch"          // .yaah-scratch inside a settled worktree
	KindMissing         Kind = "missing"          // recorded path is gone — informational
	KindQuarantined     Kind = "quarantined"      // contradictory lifecycle — never touched
	KindAdopted         Kind = "adopted"          // live scope's worktree — protected
)

// ScratchDirName is the confinement redirect directory the tools layer
// creates inside a worktree (substrate.ConfinementScratchName's contract:
// housekeeping writes land there, and the dispatch lane scrubs it before
// sealing — so inside a settled worktree it is, by definition, disposable).
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
	// Terminal is set for findings whose scope recovery classified as done
	// (Orphans): a settle fact on a terminal scope means the teardown was
	// missed and the worktree is scratch. An unadopted scope is NOT
	// terminal — it may be mid-life (a settle fact is appended before
	// teardown), so settled state alone never makes it reclaimable.
	Terminal bool
	Reason   string
}

// Report is the scan result: findings plus totals. ReclaimableBytes sums
// findings with SafeToDelete — what `janitor run` would reclaim today.
// Worktree findings count the whole directory; scratch findings are an
// informational breakdown *inside* a settled worktree (the parent finding
// already owns those bytes), never a second claim.
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
// Facts may land on the proposal's own dispatch scope (seal) or on a parent
// owner (settlement records on the host root scope, the dispatch scope
// being discarded by then), so the index joins on the payload's
// proposal_id across all owners that recorded the proposal.
type lifecycleIndex struct {
	// state maps every recorded owner to that owner's latest verb, plus the
	// bare "scope:proposal:<pid>" prefix form for lookup convenience.
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

// embeddedID extracts the proposal id from a dispatch scope id. The kernel
// builds scope ids as "scope:<owner>", and dispatch owners are proposal ids
// themselves ("proposal:<nanos>:<seq>"), so the embedded form is the
// remainder after "scope:"; "" for other scope shapes.
func (l lifecycleIndex) embeddedID(scopeID string) string {
	if rest, ok := strings.CutPrefix(scopeID, "scope:"); ok {
		if strings.HasPrefix(rest, "proposal:") {
			return rest
		}
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

	// Orphans: terminal scope, worktree on disk. The proposal lifecycle
	// decides between safe and report-only.
	for _, o := range report.Orphans {
		add(worktreeFinding(index, o.ScopeID, o.Path, o.Size, true))
	}

	// Unadopted: the scope is not terminal and the path exists but is not a
	// registered worktree of its repo — git operations against it fail, so
	// it cannot serve a live scope. Even with a settle fact recorded (the
	// fact is appended before teardown), the scope may be mid-life: settled
	// state alone never makes it reclaimable.
	for _, u := range report.Unadopted {
		add(worktreeFinding(index, u.ScopeID, u.Path, dirSize(u.Path), false))
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
// lifecycle. Only a terminal scope with a settled proposal and a
// worktree-shaped directory (one live target repo, optionally plus the
// confinement scratch) earns the safe verdict: recovery includes any
// existing path for a terminal scope in Orphans, so provenance — not the
// trace alone — must confirm the directory is the dispatch-created worktree.
func worktreeFinding(index lifecycleIndex, scopeID, path string, size int64, terminal bool) Finding {
	state := index.stateFor(scopeID)
	proposalID := index.idFor(scopeID)
	f := Finding{
		Path:       path,
		ScopeID:    scopeID,
		ProposalID: proposalID,
		Bytes:      size,
		ModTime:    modTime(path),
		Terminal:   terminal,
	}
	switch state {
	case lifecycleSettled:
		// Terminality is the load-bearing fact: a settle fact is appended
		// BEFORE teardown, so an active scope can carry one and still be
		// mid-life. Only a scope recovery classified as done (Orphans) whose
		// directory is verifiably the dispatch worktree is reclaimable.
		if !f.Terminal {
			f.Kind = KindUnsealedResidue
			f.Reason = "proposal settled, but the scope is not terminal — an active scope is never reclaimable"
			return f
		}
		if looksLikeWorktree(path) {
			f.Kind = KindSettledWorktree
			f.SafeToDelete = true
			f.Reason = fmt.Sprintf("proposal %s settled — teardown was missed; the landed delta is the record, the worktree is scratch",
				proposalName(proposalID, scopeID))
			return f
		}
		f.Kind = KindUnsealedResidue
		f.Reason = "proposal settled, but this directory does not look like a dispatch worktree — treating as unverified, do not reclaim"
		return f
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

// looksLikeWorktree is a conservative shape check for "this directory is
// a git worktree of some repo": it carries a .git entry (a file in a
// linked worktree, a directory in a main checkout). The check is
// deliberately minimal — a checkout's tracked files (README, source
// trees) are normal worktree content, and inventing a stricter shape
// would refuse real worktrees. What it does refuse is the dangerous
// impostor: a directory with no .git at all (a home directory, a build
// tree) that happens to sit at a recorded path.
//
// Provenance beyond the shape is the caller's job: the collector only
// reclaims settled worktrees that git's registry no longer lists.
func looksLikeWorktree(path string) bool {
	if path == "" {
		return false
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			// Either shape marks a git worktree root.
			return true
		}
	}
	return false
}

// ScanScratch reports the confinement scratch directories inside the
// SETTLED worktrees of an existing classification. Restricted to findings
// the janitor already owns: an occupant that recovery classified as
// unadopted may be unrelated (a home directory, an unmerged worktree of
// its own), and its scratch must not inherit the safe verdict. Separate
// from Classify so callers can show the breakdown inside a settled
// worktree's bytes.
func ScanScratch(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind != KindSettledWorktree || f.Path == "" {
			continue
		}
		scratch := filepath.Join(f.Path, ScratchDirName)
		fi, err := os.Stat(scratch)
		if err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, Finding{
			Kind:    KindScratch,
			Path:    scratch,
			ScopeID: f.ScopeID,
			Bytes:   dirSize(scratch),
			ModTime: fi.ModTime(),
			// Informational: the settled worktree finding already owns these
			// bytes and the reclaimable total counts them once.
			SafeToDelete: false,
			Reason:       "confinement redirect inside a settled worktree: housekeeping writes (build caches, temp), scrubbed at seal on the normal path",
		})
	}
	sortFindings(out)
	return out
}

// proposalLifecycle reads the sealed/settled facts once and folds them into
// per-scope lifecycle state: settled wins over sealed (the later verb is the
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

// scopeIDForProposal returns the scope-id form of a proposal id. Proposal
// ids ("proposal:<nanos>:<seq>") ARE the dispatch owners; the kernel
// scopes them to "scope:<owner>", so the joined form is
// "scope:proposal:<nanos>:<seq>".
func scopeIDForProposal(proposalID string) string {
	if proposalID == "" {
		return ""
	}
	if strings.HasPrefix(proposalID, "scope:") {
		return proposalID
	}
	return "scope:" + proposalID
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

// ErrNotATraceStore marks a store that cannot serve classification, so
// callers can distinguish a mispointed viewer from a real fault.
var ErrNotATraceStore = errors.New("janitor: not a trace store")

// SweepOptions configures a reclamation run. The zero value reclaims only
// scratch (always safe); worktree reclamation is opt-in via the age
// threshold, because a settled worktree past the threshold has had its
// chance to be claimed by a human reviewing the landed delta.
type SweepOptions struct {
	// Now overrides the clock for age computations (tests). Zero uses
	// time.Now.
	Now func() time.Time
	// SettledWorktreeAge is how long a settled worktree's directory must
	// be untouched before reclamation. Zero disables worktree reclamation
	// entirely — the safe default.
	SettledWorktreeAge time.Duration
	// MaxWorktrees caps worktree deletions per run; zero means no cap.
	MaxWorktrees int
}

// SweepTarget is one deletion the collector selected: what to remove and
// the evidence chain that justified it. Targets are classified in phases;
// phase 1 reclaims worktrees only (registered and owned by their repo, or
// no longer registered at all), phase 2 scratch directories.
type SweepTarget struct {
	Kind     Kind
	Path     string
	ScopeID  string
	Bytes    int64
	Terminal bool
	Reason   string
}

// SweepResult reports what one run reclaimed.
type SweepResult struct {
	DeletedWorktrees int
	DeletedScratch   int
	ReclaimedBytes   int64
	Failed           []SweepFailure
}

// SweepFailure records one deletion that errored, by design non-fatal: the
// audit trail carries it and the next run retries.
type SweepFailure struct {
	Path string
	Err  error
}

// Collector reclaims the safe set. The repoPath is the target repository
// whose worktree registry vouches for (or disowns) a path — a worktree
// still registered is never deleted out from under git.
type Collector struct {
	store    *shepherdtrace.SQLiteTraceStore
	registry *recovery.Report
	repoPath string
	opts     SweepOptions
	now      func() time.Time
}

// NewCollector builds a collector over a classified registry.
func NewCollector(store *shepherdtrace.SQLiteTraceStore, registry *recovery.Report, repoPath string, opts SweepOptions) *Collector {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Collector{store: store, registry: registry, repoPath: repoPath, opts: opts, now: now}
}

// Targets classifies the safe set into concrete deletions: settled
// worktrees past the age threshold (and never adopted, quarantined, or
// still registered as live worktrees), then scratch inside settled
// worktrees. Orphans whose repo is unknown or whose registry lookup fails
// are skipped — unverifiable provenance never deletes.
func (c *Collector) Targets(ctx context.Context) ([]SweepTarget, error) {
	index, err := proposalLifecycle(ctx, c.store)
	if err != nil {
		return nil, fmt.Errorf("read proposal lifecycle: %w", err)
	}

	orphanByPath := make(map[string]recovery.Orphan, len(c.registry.Orphans))
	for _, o := range c.registry.Orphans {
		orphanByPath[o.Path] = o
	}

	var targets []SweepTarget
	worktreeCount := 0
	consider := func(scopeID, path string, size int64) {
		f := worktreeFinding(index, scopeID, path, size, true)
		if f.Kind != KindSettledWorktree {
			return
		}
		// Age gate: untouched since before the threshold, or the clock
		// says the directory is younger than the settle.
		if c.opts.SettledWorktreeAge > 0 && c.age(path) < c.opts.SettledWorktreeAge {
			return
		}
		// The git registry is the authority on whether a path is still
		// live work. Ownership (recovery.WorktreeOwnedBy) is the strong
		// check: the .git file must point into the repo's worktree
		// administration. Registry membership alone is not ownership —
		// an unrelated directory sharing the registry is not live work
		// either, and must not be protected by it. A lookup failure skips
		// (unverifiable never deletes). The two checks together are the
		// adoption rule: still owned AND still listed ⇒ live scope, keep.
		orphan, ok := orphanByPath[path]
		if !ok || orphan.Repo == "" {
			return
		}
		if recovery.WorktreeOwnedBy(orphan.Repo, path) && c.registered(ctx, orphan.Repo, path) {
			return
		}
		if c.opts.MaxWorktrees > 0 && worktreeCount >= c.opts.MaxWorktrees {
			return
		}
		worktreeCount++
		targets = append(targets, SweepTarget{
			Kind: KindSettledWorktree, Path: path, ScopeID: scopeID,
			Bytes: size, Terminal: true,
			Reason: "settled proposal, teardown missed, past the age threshold, no longer a registered worktree of its repo",
		})
	}

	for _, o := range c.registry.Orphans {
		consider(o.ScopeID, o.Path, o.Size)
	}
	// Unadopted entries never enter the worktree safe set (not terminal);
	// scratch inside them is equally untrusted — see ScanScratch's
	// contract.
	for _, f := range ScanScratchFrom(c.registry, index) {
		targets = append(targets, SweepTarget{
			Kind: KindScratch, Path: f.Path, ScopeID: f.ScopeID, Bytes: f.Bytes,
			Reason: "confinement redirect inside a settled worktree: housekeeping writes, scrubbed at seal on the normal path",
		})
	}
	return targets, nil
}

// registered reports whether git's registry lists path as a worktree of
// repo, distinguishing lookup failure (deletion must refuse) from a
// genuine not-listed. Used as one half of the liveness check; the other
// is recovery.WorktreeOwnedBy (ownership via the .git file).
func (c *Collector) registered(ctx context.Context, repo, path string) bool {
	paths, err := recovery.RegisteredWorktreePaths(ctx, repo)
	if err != nil {
		return true // unverifiable: fail safe toward "live"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	for _, p := range paths {
		if p == path || p == abs {
			return true
		}
		ps, perr := os.Stat(p)
		ns, nerr := os.Stat(abs)
		if perr == nil && nerr == nil && os.SameFile(ps, ns) {
			return true
		}
	}
	return false
}

func (c *Collector) age(path string) time.Duration {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return c.now().Sub(fi.ModTime())
}

// Execute reclaims the targets in order, collecting per-path failures
// instead of aborting: one undeletable worktree must not block the rest
// of the sweep.
func (c *Collector) Execute(ctx context.Context, targets []SweepTarget) SweepResult {
	var res SweepResult
	for _, t := range targets {
		var err error
		switch t.Kind {
		case KindSettledWorktree:
			err = c.removeWorktree(ctx, t)
			if err == nil {
				res.DeletedWorktrees++
				res.ReclaimedBytes += t.Bytes
			}
		case KindScratch:
			err = os.RemoveAll(t.Path)
			if err == nil {
				res.DeletedScratch++
				// Scratch bytes were counted inside the parent worktree
				// finding only when that worktree was also deleted; when
				// it is report-only, the scratch is the sole reclaim.
				res.ReclaimedBytes += t.Bytes
			}
		}
		if err != nil {
			res.Failed = append(res.Failed, SweepFailure{Path: t.Path, Err: err})
		}
	}
	return res
}

// removeWorktree deletes one worktree through git's registry: unregister
// first (so no stale entry survives a partially-failed delete), then remove
// the directory tree. When git cannot unregister (a hand-cleared or
// corrupted registry), the directory tree is still the payload — removing
// it is the reclaim, and git's bookkeeping self-heals via
// `git worktree prune`.
func (c *Collector) removeWorktree(ctx context.Context, t SweepTarget) error {
	if err := gitWorktreeRemove(ctx, c.repoPath, t.Path); err != nil {
		if rmErr := os.RemoveAll(t.Path); rmErr != nil {
			return fmt.Errorf("git remove: %v; remove tree: %w", err, rmErr)
		}
	}
	return nil
}

// ScanScratchFrom classifies scratch directories for the collector from
// the registry + lifecycle index, without re-reading the store: the
// collector already holds both. It mirrors ScanScratch's restriction —
// settled worktrees only.
func ScanScratchFrom(registry *recovery.Report, index lifecycleIndex) []Finding {
	findings := make([]Finding, 0, len(registry.Orphans))
	for _, o := range registry.Orphans {
		findings = append(findings, worktreeFinding(index, o.ScopeID, o.Path, o.Size, true))
	}
	return ScanScratch(findings)
}

// gitWorktreeRemove unregisters and deletes one worktree through git:
// `git -C repo worktree remove <path>`. A missing repo or a path git does
// not know fails, and the caller's tree-removal fallback covers it.
func gitWorktreeRemove(ctx context.Context, repoPath, path string) error {
	if repoPath == "" {
		return fmt.Errorf("no repository to unregister the worktree against")
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "remove", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// LogResult emits one structured audit line per sweep. The janitor's audit
// trail is slog (janitor-plan.md §4): who reclaimed what, when, and how
// much — enough to reconstruct any deletion from the logs alone.
func LogResult(res SweepResult) {
	slog.Info("janitor: sweep complete",
		"worktrees", res.DeletedWorktrees,
		"scratch", res.DeletedScratch,
		"reclaimed_bytes", res.ReclaimedBytes,
		"failed", len(res.Failed))
	for _, f := range res.Failed {
		slog.Warn("janitor: deletion failed; will retry next sweep", "path", f.Path, "err", f.Err)
	}
}
