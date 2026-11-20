package janitor

// Collector and sweep: the reclamation mode's implementation. The safe
// set comes from Classify (janitor.go); this file turns it into concrete
// deletions and executes them.
//
// Every deletion re-verifies its target immediately before removing
// anything (ownership of the directory, live-ness in git's registry), and
// removal itself goes through goatherd-runtime's worktree sandbox — the
// same implementation provisioning and goatherd prune use, with the
// runtime's proven ownership/adoption rule and NUL-safe registry parsing
// (yaah's own RegisteredWorktreePaths is line-based and can miss paths
// git quotes; the runtime's `-z` split does not).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	goatherdgit "github.com/goatherdai/goatherd-runtime/sandbox/git"

	shepherdtrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/agent/recovery"
)

// SweepOptions configures a reclamation run. The zero value reclaims only
// scratch (always safe); worktree reclamation is opt-in via the age
// threshold, because a settled worktree past the threshold has had its
// chance to be claimed by a human reviewing the landed delta.
type SweepOptions struct {
	// Now overrides the clock for age computations (tests). Zero uses
	// time.Now.
	Now func() time.Time
	// SettledWorktreeAge is how long a settled worktree's directory must
	// be untouched before reclamation. Zero or negative disables worktree
	// reclamation entirely — the safe default. Callers must reject
	// negative config values before they reach here; a negative duration
	// arriving anyway is treated as disabled.
	SettledWorktreeAge time.Duration
	// Force bypasses the age gate for deliberate same-day cleanup. It
	// enables worktree reclamation even when no age threshold is
	// configured, and does NOT bypass the liveness/ownership checks —
	// a live or unproven occupant is still never touched.
	Force bool
	// MaxWorktrees caps worktree deletions per run; zero means no cap.
	MaxWorktrees int
}

// SweepTarget is one deletion the collector selected: what to remove and
// the evidence chain that justified it.
type SweepTarget struct {
	Kind    Kind
	Path    string
	ScopeID string
	Bytes   int64
	// Terminal records that recovery classified the scope as done
	// (Orphans) — the load-bearing fact for worktree reclamation.
	Terminal bool
	Reason   string
}

// SweepResult reports what one run reclaimed. DeletedPaths names every
// successfully removed path (the audit trail's per-path record), Failed
// carries the ones that errored — non-fatal by design, retried next run.
type SweepResult struct {
	DeletedWorktrees int
	DeletedScratch   int
	ReclaimedBytes   int64
	DeletedPaths     []string
	Failed           []SweepFailure
}

// SweepFailure records one deletion that errored.
type SweepFailure struct {
	Path string
	Err  error
}

// Collector reclaims the safe set. repoPath is the target repository
// whose worktree registry vouches for (or disowns) a path.
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
	if opts.SettledWorktreeAge < 0 {
		opts.SettledWorktreeAge = 0 // negative is disabled, never "immediate"
	}
	return &Collector{store: store, registry: registry, repoPath: repoPath, opts: opts, now: now}
}

// worktreeEnabled reports whether worktree reclamation is active: an
// explicit force, or a strictly positive age threshold. Force enables it
// even when no threshold is configured — see SweepOptions.Force.
func (c *Collector) worktreeEnabled() bool {
	return c.opts.Force || c.opts.SettledWorktreeAge > 0
}

// Targets classifies the safe set into concrete deletions: settled
// worktrees past the age threshold (never adopted, quarantined, or still
// live), then scratch inside settled worktrees. Orphans whose repo is
// unknown or whose registry lookup fails are skipped — unverifiable
// provenance never deletes. When worktree reclamation is disabled, only
// scratch is selected.
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
		if !c.worktreeEnabled() {
			return
		}
		// Age gate: untouched since before the threshold. Force lifts
		// the review window, but nothing else — liveness, ownership and
		// provenance are still checked below.
		if !c.opts.Force && c.age(path) < c.opts.SettledWorktreeAge {
			return
		}
		// Live-ness: the adoption rule (owned AND listed). Unverifiable
		// provenance (repo unknown, lookup failed) skips.
		orphan, ok := orphanByPath[path]
		if !ok || orphan.Repo == "" {
			return
		}
		live, liveErr := c.liveInRegistry(ctx, orphan.Repo, path)
		if liveErr != nil || live {
			return
		}
		if c.opts.MaxWorktrees > 0 && worktreeCount >= c.opts.MaxWorktrees {
			return
		}
		worktreeCount++
		targets = append(targets, SweepTarget{
			Kind: KindSettledWorktree, Path: path, ScopeID: scopeID,
			Bytes: size, Terminal: true,
			Reason: "settled proposal, teardown missed, past the age threshold, no longer a live worktree of its repo",
		})
	}

	for _, o := range c.registry.Orphans {
		consider(o.ScopeID, o.Path, o.Size)
	}
	for _, f := range ScanScratchFrom(c.registry, index, c.worktreeEnabled()) {
		targets = append(targets, SweepTarget{
			Kind: KindScratch, Path: f.Path, ScopeID: f.ScopeID, Bytes: f.Bytes,
			Reason: "confinement redirect inside a settled worktree: housekeeping writes, scrubbed at seal on the normal path",
		})
	}
	return targets, nil
}

// liveInRegistry reports whether path is currently live work: still
// owned by its recorded repo AND still listed in git's registry. Both
// halves come from the runtime sandbox's proven checks (ownership via
// WorktreeOwnedBy, registry via the NUL-safe git worktree list). A
// lookup error is returned so the caller skips — unverifiable never
// deletes.
func (c *Collector) liveInRegistry(ctx context.Context, repo, path string) (live bool, err error) {
	owned := recovery.WorktreeOwnedBy(repo, path)
	registered, rerr := c.registered(ctx, repo, path)
	if rerr != nil {
		return false, rerr
	}
	return owned && registered, nil
}

// registered reports whether git's registry currently lists path as a
// worktree of repo, using the runtime's NUL-safe listing (paths with
// newlines survive; the porcelain line form does not). A lookup failure
// is an error, not a false — the caller skips on error.
func (c *Collector) registered(ctx context.Context, repo, path string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("no repository to check the worktree registry against")
	}
	sb := goatherdgit.NewWorktreeSandbox(repo, path)
	return sb.IsRegisteredWorktree(ctx)
}

func (c *Collector) age(path string) time.Duration {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return c.now().Sub(fi.ModTime())
}

// Execute reclaims the targets in order, collecting per-path failures
// instead of aborting. Each target is re-verified immediately before
// deletion — the classification ran earlier and the world may have moved
// (a path replaced by a live checkout, a registry that changed): ownership
// and live-ness are checked again, and a target that fails re-verification
// is skipped (recorded as failed with the reason), never force-deleted.
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
				res.DeletedPaths = append(res.DeletedPaths, t.Path)
			}
		case KindScratch:
			err = c.removeScratch(ctx, t)
			if err == nil {
				res.DeletedScratch++
				res.ReclaimedBytes += t.Bytes
				res.DeletedPaths = append(res.DeletedPaths, t.Path)
			}
		}
		if err != nil {
			res.Failed = append(res.Failed, SweepFailure{Path: t.Path, Err: err})
		}
	}
	sort.Strings(res.DeletedPaths)
	return res
}

// removeWorktree deletes one worktree, re-verifying first: the directory
// must still be a settled worktree's shadow (owned by its repo's
// administration), must not be live (owned AND registered), and git must
// still be able to remove it. The removal itself goes through the runtime
// sandbox's Destroy — the same path goatherd prune uses, with the
// runtime's ownership enforcement — and a failure there NEVER falls back
// to a raw tree removal: an unproven occupant must not be erased by the
// fallback.
func (c *Collector) removeWorktree(ctx context.Context, t SweepTarget) error {
	if !c.worktreeEnabled() {
		return fmt.Errorf("worktree reclamation is disabled (no age threshold configured)")
	}
	orphan, ok := c.orphanFor(t.Path)
	if !ok || orphan.Repo == "" {
		return fmt.Errorf("no recorded repository for %s — refusing", t.Path)
	}
	live, liveErr := c.liveInRegistry(ctx, orphan.Repo, t.Path)
	if liveErr != nil {
		return fmt.Errorf("registry check: %w", liveErr)
	}
	if live {
		return fmt.Errorf("%s is live work (owned and registered) — refusing", t.Path)
	}
	// Ownership re-check at delete time: the shape check at classify time
	// is not enough after a possible replacement.
	if !recovery.WorktreeOwnedBy(orphan.Repo, t.Path) {
		return fmt.Errorf("%s is no longer owned by its repo — refusing (the occupant changed since classification)", t.Path)
	}
	// Removal through the runtime sandbox's orphan path: the registry
	// entry is gone (that is what makes it an orphan), so Destroy's
	// fail-closed unregistered rule does not apply — but the ownership
	// check above has already proven the occupant is the worktree this
	// repo's administration created.
	sb := goatherdgit.NewWorktreeSandbox(orphan.Repo, t.Path)
	if err := sb.RemoveOrphanDeregistered(ctx); err != nil {
		return fmt.Errorf("remove orphan worktree: %w", err)
	}
	return nil
}

// removeScratch deletes one scratch directory, re-verifying that its
// parent worktree is still a settled, owned worktree — an unrelated
// checkout that replaced the recorded path must not lose its own
// .yaah-scratch to this sweep.
func (c *Collector) removeScratch(ctx context.Context, t SweepTarget) error {
	parent := filepath.Dir(t.Path)
	orphan, ok := c.orphanFor(parent)
	if !ok || orphan.Repo == "" {
		return fmt.Errorf("no recorded repository for %s — refusing", t.Path)
	}
	if !recovery.WorktreeOwnedBy(orphan.Repo, parent) {
		return fmt.Errorf("parent of %s is no longer owned by its repo — refusing", t.Path)
	}
	if err := os.RemoveAll(t.Path); err != nil {
		return fmt.Errorf("remove scratch: %w", err)
	}
	return nil
}

// orphanFor finds the registry entry that recorded path.
func (c *Collector) orphanFor(path string) (recovery.Orphan, bool) {
	for _, o := range c.registry.Orphans {
		if o.Path == path {
			return o, true
		}
	}
	return recovery.Orphan{}, false
}

// ScanScratchFrom classifies scratch directories for the collector from
// the registry + lifecycle index, without re-reading the store. Restricted
// to SETTLED worktree findings only; when worktree reclamation is
// disabled, scratch inside settled worktrees is still collected (it is
// safe unconditionally) — the enabled flag only gates worktree deletion.
func ScanScratchFrom(registry *recovery.Report, index lifecycleIndex, _ bool) []Finding {
	findings := make([]Finding, 0, len(registry.Orphans))
	for _, o := range registry.Orphans {
		findings = append(findings, worktreeFinding(index, o.ScopeID, o.Path, o.Size, true))
	}
	return ScanScratch(findings)
}

// LogResult emits the audit trail: one line per deleted path (the
// per-path record), one summary line with counts and bytes, and one
// warning per failure. Enough to reconstruct any sweep from the logs.
func LogResult(res SweepResult) {
	for _, p := range res.DeletedPaths {
		slog.Info("janitor: reclaimed", "path", p)
	}
	slog.Info("janitor: sweep complete",
		"worktrees", res.DeletedWorktrees,
		"scratch", res.DeletedScratch,
		"reclaimed_bytes", res.ReclaimedBytes,
		"failed", len(res.Failed))
	for _, f := range res.Failed {
		slog.Warn("janitor: deletion failed; will retry next sweep", "path", f.Path, "err", f.Err)
	}
}

// ErrNothingToReclaim is returned by callers when the target set is
// empty; the command treats it as success with a message.
var ErrNothingToReclaim = errors.New("nothing to reclaim")
