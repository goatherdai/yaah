package yaah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/janitor"
	"github.com/goatherdai/yaah/internal/tracestore"
)

// janitorCmd reports dispatch residue — stranded worktrees, confinement
// scratch, leaked containers — classified from the trace's proposal
// lifecycle facts (janitor-plan.md). Scan is read-only: `janitor run`
// (the reclamation mode) is a later phase; nothing deletes here.
var janitorCmd = &cobra.Command{
	Use:   "janitor",
	Short: "Scan dispatch residue: stranded worktrees, scratch, containers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("janitor: pick a mode — `yaah janitor scan`")
	},
}

var janitorScanCmd = &cobra.Command{
	Use:   "scan",
	Short: "Classify and report dispatch residue (read-only)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		report, cleanup, err := janitorScan(cmd.Context())
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return err
		}
		printJanitorReport(cmd.OutOrStdout(), report)
		return nil
	},
}

var janitorRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Reclaim the safe residue set: scratch always, settled worktrees past the age threshold",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		force, _ := cmd.Flags().GetBool("force")
		return janitorRun(cmd.Context(), cmd.OutOrStdout(), dryRun, force)
	},
}

// janitorScan wires the three read-only sources: the trace store (proposal
// lifecycle facts), recovery's registry (scope ids and worktree paths), and
// the filesystem. The trace opens READ-ONLY: a scan creates no directory,
// writes no schema, and touches no journal state — a viewer pointed at a
// wrong path fails loudly instead of materializing a store, and a missing
// trace reports nothing to clean rather than conjuring one.
func janitorScan(ctx context.Context) (*janitor.Report, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("janitor: load config: %w", err)
	}
	traceCfg := cfg.Agent.Default.TraceStoreConfig()
	if traceCfg.IsSQLite() {
		if _, statErr := os.Stat(traceCfg.DSN); errors.Is(statErr, os.ErrNotExist) {
			return &janitor.Report{GeneratedAt: time.Now()}, nil, nil
		}
	}
	store, err := tracestore.OpenReadOnly(traceCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("janitor: trace store (%s): %w", traceCfg.Describe(), err)
	}
	cleanup := func() { store.Close(context.Background()) }

	repoPath := cfg.Agent.Default.RetainedRepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	_, registry, rerr := recovery.Recover(ctx, store, repoPath, true, nil)
	if rerr != nil {
		return nil, cleanup, fmt.Errorf("janitor: recovery: %w", rerr)
	}

	report, err := janitor.Classify(ctx, store, registry)
	if err != nil {
		return nil, cleanup, fmt.Errorf("janitor: classify: %w", err)
	}
	report.Findings = append(report.Findings, janitor.ScanScratch(report.Findings)...)
	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Kind != report.Findings[j].Kind {
			return report.Findings[i].Kind < report.Findings[j].Kind
		}
		return report.Findings[i].Path < report.Findings[j].Path
	})
	// Classify totals: the worktree finding already owns the scratch bytes
	// inside it, so scratch findings are breakdown, not a second claim.
	report.ReclaimableBytes, report.TotalBytes = 0, 0
	for _, f := range report.Findings {
		if f.Kind == janitor.KindScratch {
			continue
		}
		report.TotalBytes += f.Bytes
		if f.SafeToDelete {
			report.ReclaimableBytes += f.Bytes
		}
	}
	return report, cleanup, nil
}

// janitorRun reclaims the safe set: scratch directories always, settled
// worktrees only past the configured age threshold (a settled worktree is
// a landed delta's shadow — the age gate is the human-review window). The
// dry-run flag reports what would go; force lifts the age gate for
// deliberate same-day cleanup.
func janitorRun(ctx context.Context, w io.Writer, dryRun, force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("janitor: load config: %w", err)
	}
	traceCfg := cfg.Agent.Default.TraceStoreConfig()
	if traceCfg.IsSQLite() {
		if _, statErr := os.Stat(traceCfg.DSN); errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintln(w, "no trace store — nothing to reclaim.")
			return nil
		}
	}
	store, err := tracestore.OpenReadOnly(traceCfg)
	if err != nil {
		return fmt.Errorf("janitor: trace store (%s): %w", traceCfg.Describe(), err)
	}
	defer store.Close(context.Background())

	repoPath := cfg.Agent.Default.RetainedRepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	_, registry, rerr := recovery.Recover(ctx, store, repoPath, true, nil)
	if rerr != nil {
		return fmt.Errorf("janitor: recovery: %w", rerr)
	}

	opts := janitor.SweepOptions{SettledWorktreeAge: 0}
	if !force {
		if cfg.Agent.Default.Janitor.SettledWorktreeAge != "" {
			d, derr := time.ParseDuration(cfg.Agent.Default.Janitor.SettledWorktreeAge)
			if derr != nil {
				return fmt.Errorf("janitor: settled_worktree_age %q: %w", cfg.Agent.Default.Janitor.SettledWorktreeAge, derr)
			}
			opts.SettledWorktreeAge = d
		}
	}
	collector := janitor.NewCollector(store, registry, repoPath, opts)

	targets, err := collector.Targets(ctx)
	if err != nil {
		return fmt.Errorf("janitor: classify: %w", err)
	}

	if dryRun {
		if len(targets) == 0 {
			fmt.Fprintln(w, "nothing to reclaim.")
			return nil
		}
		fmt.Fprintln(w, "dry-run: would reclaim:")
		var bytes int64
		for _, t := range targets {
			fmt.Fprintf(w, "  %-12s %10s  %s\n    %s\n", t.Kind, humanBytes(t.Bytes), t.Path, t.Reason)
			bytes += t.Bytes
		}
		fmt.Fprintf(w, "total: %s across %d target(s); nothing was deleted (dry-run).\n", humanBytes(bytes), len(targets))
		return nil
	}

	if len(targets) == 0 {
		fmt.Fprintln(w, "nothing to reclaim.")
		return nil
	}

	// Single-flight per backend: a second concurrent run defers to the
	// first, exactly like the retention sweep's guard.
	key := traceCfg.Backend + "|" + traceCfg.DSN
	claimed, done := janitorClaim(key)
	if !claimed {
		fmt.Fprintln(w, "another janitor run is in flight; this one defers.")
		return nil
	}
	defer done()

	res := collector.Execute(ctx, targets)
	janitor.LogResult(res)
	fmt.Fprintf(w, "reclaimed %s: %d worktree(s), %d scratch dir(s)",
		humanBytes(res.ReclaimedBytes), res.DeletedWorktrees, res.DeletedScratch)
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, "; %d failed (see logs, retried next run)", len(res.Failed))
	}
	fmt.Fprintln(w)
	return nil
}

// janitorClaim implements the process-wide single-flight guard: the first
// caller for a store key claims it, later callers defer.
func janitorClaim(key string) (claimed bool, done func()) {
	janitorMu.Lock()
	defer janitorMu.Unlock()
	if janitorRunning[key] {
		return false, func() {}
	}
	janitorRunning[key] = true
	return true, func() {
		janitorMu.Lock()
		janitorRunning[key] = false
		janitorMu.Unlock()
	}
}

var (
	janitorMu      sync.Mutex
	janitorRunning = map[string]bool{}
)

// printJanitorReport renders the classification, safe set first, with a
// one-line reason per finding — the evidence, not just the verdict.
func printJanitorReport(w io.Writer, report *janitor.Report) {
	fmt.Fprintf(w, "janitor scanned %d scope(s) (%d root(s)) at %s\n",
		report.Scopes, report.Roots, report.GeneratedAt.Format("2006-01-02 15:04:05"))
	if len(report.Findings) == 0 {
		fmt.Fprintln(w, "no dispatch residue found.")
		return
	}

	order := map[janitor.Kind]int{
		janitor.KindScratch:         0,
		janitor.KindSettledWorktree: 1,
		janitor.KindSealedUnsettled: 2,
		janitor.KindUnsealedResidue: 3,
		janitor.KindAdopted:         4,
		janitor.KindMissing:         5,
		janitor.KindQuarantined:     6,
	}
	sorted := make([]janitor.Finding, len(report.Findings))
	copy(sorted, report.Findings)
	sort.Slice(sorted, func(i, j int) bool {
		if order[sorted[i].Kind] != order[sorted[j].Kind] {
			return order[sorted[i].Kind] < order[sorted[j].Kind]
		}
		return sorted[i].Bytes > sorted[j].Bytes
	})

	for _, f := range sorted {
		verdict := "report"
		if f.SafeToDelete {
			verdict = "reclaimable"
		}
		fmt.Fprintf(w, "  %-12s [%s] %10s  %s\n    scope %s — %s\n",
			f.Kind, verdict, humanBytes(f.Bytes), f.Path, scopeOrDash(f.ScopeID), f.Reason)
	}
	fmt.Fprintf(w, "\ntotal residue: %s — reclaimable now: %s (scratch + settled worktrees)\n",
		humanBytes(report.TotalBytes), humanBytes(report.ReclaimableBytes))
	fmt.Fprintln(w, "reclamation (`janitor run`) is a later phase; nothing was deleted.")
}

func scopeOrDash(scopeID string) string {
	if strings.TrimSpace(scopeID) == "" {
		return "-"
	}
	return scopeID
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
