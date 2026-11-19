package yaah

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	// A missing trace is an empty report, not a reason to create one.
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
