package janitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shepherdtrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/agent/recovery"
	"github.com/goatherdai/yaah/internal/dispatch"
)

// The classifier's decision table, exercised against a real trace store:
// the lifecycle facts the dispatch lane appends (sealed/settled on the
// proposal's owner) joined with recovery's registry buckets.

func newTestStore(t *testing.T) *shepherdtrace.SQLiteTraceStore {
	t.Helper()
	store, err := shepherdtrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

// appendProposalFact appends a lifecycle fact exactly the way the dispatch
// lane does: SchemaProposalSealed/Settled with a proposal_id payload, on an
// owner path.
func appendProposalFact(t *testing.T, store *shepherdtrace.SQLiteTraceStore, owner, schemaRef, proposalID string) {
	t.Helper()
	payload := map[string]any{}
	if proposalID != "" {
		payload["proposal_id"] = proposalID
	}
	if _, err := store.Append(context.Background(), shepherdtrace.TrustedAppendContext, shepherdtrace.AppendBatch{
		AppendIntentID: "intent:" + schemaRef + ":" + owner + ":" + proposalID,
		Groups: []shepherdtrace.AppendGroup{{
			TraceOwnerID: owner,
			FactDrafts: []shepherdtrace.RecordDraft{{
				Mode:      shepherdtrace.Capture,
				SchemaRef: schemaRef,
				Payload:   payload,
			}},
		}},
	}); err != nil {
		t.Fatalf("append %s on %s: %v", schemaRef, owner, err)
	}
}

func makeWorktree(t *testing.T, path string, scratchBytes int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "internal"), 0o700); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	if scratchBytes > 0 {
		scratch := filepath.Join(path, ScratchDirName, "build-cache")
		if err := os.MkdirAll(scratch, 0o700); err != nil {
			t.Fatalf("mkdir scratch: %v", err)
		}
		if err := os.WriteFile(filepath.Join(scratch, "cache.bin"), make([]byte, scratchBytes), 0o600); err != nil {
			t.Fatalf("write scratch: %v", err)
		}
	}
}

// TestClassifyDecisionTable walks the full decision table: settled worktrees
// are the safe set, sealed-unsettled and unsealed residue are report-only,
// adopted is protected, missing is informational, and quarantine is never
// touched.
func TestClassifyDecisionTable(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()

	paths := map[string]string{
		"settled":  filepath.Join(dir, "wt-settled"),
		"sealed":   filepath.Join(dir, "wt-sealed"),
		"unsealed": filepath.Join(dir, "wt-unsealed"),
		"adopted":  filepath.Join(dir, "wt-adopted"),
		"missing":  filepath.Join(dir, "wt-missing"),
	}
	for _, p := range paths {
		makeWorktree(t, p, 0)
	}
	// Lifecycle facts: one proposal settled, one sealed only, one with no
	// facts at all. Facts land on the dispatch scope owner, matching the
	// dispatch lane's scope.AppendFact.
	appendProposalFact(t, store, "scope:proposal:111:1", dispatch.SchemaProposalSealed, "111")
	appendProposalFact(t, store, "scope:proposal:111:1", dispatch.SchemaProposalSettled, "111")
	appendProposalFact(t, store, "scope:proposal:222:2", dispatch.SchemaProposalSealed, "222")

	registry := &recovery.Report{
		Scopes:  5,
		Roots:   1,
		Adopted: []string{paths["adopted"]},
		Orphans: []recovery.Orphan{
			{ScopeID: "scope:proposal:111:1", Path: paths["settled"], Size: 100},
		},
		Unadopted: []recovery.Unadopted{
			{ScopeID: "scope:proposal:222:2", Path: paths["sealed"]},
			{ScopeID: "scope:proposal:333:3", Path: paths["unsealed"]},
		},
		Missing:     []recovery.Missing{{ScopeID: "scope:proposal:444:4", Path: paths["missing"]}},
		Quarantined: []recovery.Quarantined{{Owner: "scope:odd"}},
	}

	report, err := Classify(context.Background(), store, registry)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	byKind := map[Kind][]Finding{}
	for _, f := range report.Findings {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}

	if got := len(byKind[KindSettledWorktree]); got != 1 {
		t.Fatalf("settled-worktree findings = %d, want 1", got)
	}
	settled := byKind[KindSettledWorktree][0]
	if settled.Path != paths["settled"] || !settled.SafeToDelete {
		t.Errorf("settled finding = %+v, want the settled path marked safe", settled)
	}
	if settled.ProposalID != "111" {
		t.Errorf("settled proposal id = %q, want 111", settled.ProposalID)
	}

	if got := len(byKind[KindSealedUnsettled]); got != 1 {
		t.Fatalf("sealed-unsettled findings = %d, want 1", got)
	}
	sealed := byKind[KindSealedUnsettled][0]
	if sealed.SafeToDelete {
		t.Errorf("sealed-unsettled marked safe — v1 is report-only for sealed work")
	}
	if sealed.ProposalID != "222" {
		t.Errorf("sealed proposal id = %q, want 222", sealed.ProposalID)
	}

	if got := len(byKind[KindUnsealedResidue]); got != 1 {
		t.Fatalf("unsealed-residue findings = %d, want 1", got)
	}
	if byKind[KindUnsealedResidue][0].SafeToDelete {
		t.Errorf("unsealed residue marked safe — it may hold the only copy of the work")
	}

	if got := len(byKind[KindAdopted]); got != 1 {
		t.Fatalf("adopted findings = %d, want 1", got)
	}
	if byKind[KindAdopted][0].SafeToDelete {
		t.Errorf("adopted worktree marked safe — live work is never touched")
	}

	if got := len(byKind[KindMissing]); got != 1 {
		t.Fatalf("missing findings = %d, want 1", got)
	}
	if got := len(byKind[KindQuarantined]); got != 1 {
		t.Fatalf("quarantined findings = %d, want 1", got)
	}
}

// TestScratchScan pins the scratch contract: the confinement redirect is
// reported for every on-disk non-adopted worktree (safe), and never for
// adopted ones.
func TestScratchScan(t *testing.T) {
	dir := t.TempDir()
	withScratch := filepath.Join(dir, "wt-with-scratch")
	without := filepath.Join(dir, "wt-without")
	adopted := filepath.Join(dir, "wt-adopted")
	makeWorktree(t, withScratch, 4096)
	makeWorktree(t, without, 0)
	makeWorktree(t, adopted, 2048)

	findings := []Finding{
		{Kind: KindSettledWorktree, Path: withScratch},
		{Kind: KindSealedUnsettled, Path: without},
		{Kind: KindAdopted, Path: adopted},
	}
	scratch := ScanScratch(findings)
	if len(scratch) != 1 {
		t.Fatalf("scratch findings = %d, want 1 (only the settled worktree's)", len(scratch))
	}
	if !scratch[0].SafeToDelete || scratch[0].Bytes < 4096 {
		t.Errorf("scratch finding = %+v; want safe and ≥4096 bytes", scratch[0])
	}
	if !strings.Contains(scratch[0].Path, ScratchDirName) {
		t.Errorf("scratch path %q does not name the scratch dir", scratch[0].Path)
	}
}

// TestLookupByEmbeddedProposalID covers the cross-index: settle facts land
// on the parent owner, seal facts on the dispatch scope — the classifier
// must resolve state for a scope id from either side.
func TestLookupByEmbeddedProposalID(t *testing.T) {
	store := newTestStore(t)
	// Sealed on the dispatch scope; settled on a different owner entirely
	// (the parent), both carrying the proposal id.
	appendProposalFact(t, store, "scope:proposal:555:1", dispatch.SchemaProposalSealed, "555")
	appendProposalFact(t, store, "owner:parent:main", dispatch.SchemaProposalSettled, "555")

	index, err := proposalLifecycle(context.Background(), store)
	if err != nil {
		t.Fatalf("proposalLifecycle: %v", err)
	}
	if got := index.stateFor("scope:proposal:555:1"); got != lifecycleSettled {
		t.Errorf("stateFor(dispatch scope) = %v, want settled", got)
	}
	if got := index.idFor("scope:proposal:555:1"); got != "555" {
		t.Errorf("idFor(dispatch scope) = %q, want 555", got)
	}
	if got := index.stateFor("scope:proposal:999:9"); got != lifecycleUnknown {
		t.Errorf("stateFor(unknown scope) = %v, want unknown", got)
	}
}

// TestEmptyTraceClassifiesQuietly: no facts, no residue findings — the scan
// must not invent entries for an empty ledger.
func TestEmptyTraceClassifiesQuietly(t *testing.T) {
	store := newTestStore(t)
	report, err := Classify(context.Background(), store, &recovery.Report{Scopes: 0, Roots: 0})
	if err != nil {
		t.Fatalf("Classify on empty trace: %v", err)
	}
	if len(report.Findings) != 0 || report.TotalBytes != 0 {
		t.Errorf("empty trace produced findings: %+v", report.Findings)
	}
}

// modTime sanity: the finding carries the worktree's mtime, which is what
// the age thresholds (J2) will gate on.
func TestFindingCarriesModTime(t *testing.T) {
	dir := t.TempDir()
	makeWorktree(t, dir, 0)
	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if mt := modTime(dir); mt.Before(before) {
		t.Errorf("modTime %v predates the write at %v", mt, before)
	}
}
