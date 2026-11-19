package janitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// makeWorktree creates a directory with the shape of a real dispatch
// worktree (a git repo root, optionally plus the confinement scratch),
// because only the real shape earns the safe verdict.
func makeWorktree(t *testing.T, path string, scratchBytes int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
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

// TestClassifyDecisionTable walks the full decision table: settled terminal
// worktrees are the safe set, active scopes with settle facts are
// report-only, sealed-unsettled and unsealed residue are report-only,
// adopted is protected, missing is informational, quarantine is never
// touched.
func TestClassifyDecisionTable(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()

	paths := map[string]string{
		"settled":       filepath.Join(dir, "wt-settled"),
		"active-settle": filepath.Join(dir, "wt-active-settle"),
		"sealed":        filepath.Join(dir, "wt-sealed"),
		"unsealed":      filepath.Join(dir, "wt-unsealed"),
		"adopted":       filepath.Join(dir, "wt-adopted"),
		"missing":       filepath.Join(dir, "wt-missing"),
	}
	for _, p := range paths {
		makeWorktree(t, p, 0)
	}
	// Lifecycle facts: one proposal settled (terminal scope), one settled
	// fact on an ACTIVE scope (the fact is appended before teardown, so a
	// failed teardown or refused adoption leaves exactly this state), one
	// sealed only, one with no facts at all. Facts land on the dispatch
	// scope owner, matching the dispatch lane's scope.AppendFact; the
	// settle for the active scope ALSO lands on a parent owner, exercising
	// the cross-owner join. Scope ids are "scope:" + proposal id, and the
	// proposal id is "proposal:<nanos>:<seq>".
	appendProposalFact(t, store, "scope:proposal:111:1", dispatch.SchemaProposalSealed, "proposal:111:1")
	appendProposalFact(t, store, "scope:proposal:111:1", dispatch.SchemaProposalSettled, "proposal:111:1")
	appendProposalFact(t, store, "scope:proposal:222:2", dispatch.SchemaProposalSealed, "proposal:222:2")
	appendProposalFact(t, store, "owner:parent:main", dispatch.SchemaProposalSettled, "proposal:333:3")
	appendProposalFact(t, store, "scope:proposal:333:3", dispatch.SchemaProposalSealed, "proposal:333:3")

	registry := &recovery.Report{
		Scopes:  6,
		Roots:   1,
		Adopted: []string{paths["adopted"]},
		Orphans: []recovery.Orphan{
			{ScopeID: "scope:proposal:111:1", Path: paths["settled"], Size: 100},
		},
		Unadopted: []recovery.Unadopted{
			{ScopeID: "scope:proposal:333:3", Path: paths["active-settle"]}, // active: settled fact, not terminal
			{ScopeID: "scope:proposal:222:2", Path: paths["sealed"]},
			{ScopeID: "scope:proposal:444:4", Path: paths["unsealed"]},
		},
		Missing:     []recovery.Missing{{ScopeID: "scope:proposal:555:5", Path: paths["missing"]}},
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
	if settled.ProposalID != "proposal:111:1" {
		t.Errorf("settled proposal id = %q, want proposal:111:1", settled.ProposalID)
	}

	// The active scope with a settle fact must NOT be in the safe set: the
	// settle fact is appended before teardown, so it cannot by itself
	// mean "the scope is done".
	if got := len(byKind[KindUnsealedResidue]); got != 2 {
		t.Fatalf("unsealed-residue findings = %d, want 2 (active-settle + unsealed)", got)
	}
	for _, f := range byKind[KindUnsealedResidue] {
		if f.SafeToDelete {
			t.Errorf("residue %s marked safe — active scopes are never reclaimable", f.Path)
		}
	}

	if got := len(byKind[KindSealedUnsettled]); got != 1 {
		t.Fatalf("sealed-unsettled findings = %d, want 1", got)
	}
	sealed := byKind[KindSealedUnsettled][0]
	if sealed.SafeToDelete {
		t.Errorf("sealed-unsettled marked safe — v1 is report-only for sealed work")
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

// TestUnrelatedOccupantNotReclaimable pins the provenance guard: a settled
// proposal whose recorded path now holds an unrelated directory (recovery
// includes any existing path) must not be labeled safe.
func TestUnrelatedOccupantNotReclaimable(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()

	// A plain home-like directory at the recorded path: no git, extra files.
	occupant := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(occupant, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupant, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	appendProposalFact(t, store, "scope:proposal:666:1", dispatch.SchemaProposalSealed, "proposal:666:1")
	appendProposalFact(t, store, "scope:proposal:666:1", dispatch.SchemaProposalSettled, "proposal:666:1")

	report, err := Classify(context.Background(), store, &recovery.Report{
		Orphans: []recovery.Orphan{{ScopeID: "scope:proposal:666:1", Path: occupant, Size: 10}},
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(report.Findings))
	}
	f := report.Findings[0]
	if f.Kind != KindUnsealedResidue || f.SafeToDelete {
		t.Errorf("finding = %+v; want unverified residue, never safe", f)
	}
	if !strings.Contains(f.Reason, "does not look like a dispatch worktree") {
		t.Errorf("reason %q does not explain the refusal", f.Reason)
	}
}

// TestScratchRestrictedToSettledWorktrees pins the provenance contract for
// the confinement redirect: only scratch inside a settled worktree is
// reported (and as breakdown, never a second reclaimable claim). Scratch
// inside an active or untrusted occupant is not reported at all.
func TestScratchRestrictedToSettledWorktrees(t *testing.T) {
	dir := t.TempDir()
	settled := filepath.Join(dir, "wt-settled")
	active := filepath.Join(dir, "wt-active")
	adopted := filepath.Join(dir, "wt-adopted")
	makeWorktree(t, settled, 4096)
	makeWorktree(t, active, 2048)
	makeWorktree(t, adopted, 1024)

	findings := []Finding{
		{Kind: KindSettledWorktree, Path: settled, SafeToDelete: true, Bytes: 100},
		{Kind: KindUnsealedResidue, Path: active},
		{Kind: KindAdopted, Path: adopted},
	}
	scratch := ScanScratch(findings)
	if len(scratch) != 1 {
		t.Fatalf("scratch findings = %d, want 1 (only inside the settled worktree)", len(scratch))
	}
	if scratch[0].Path != filepath.Join(settled, ScratchDirName) {
		t.Errorf("scratch path = %q, want the settled worktree's", scratch[0].Path)
	}
	if scratch[0].SafeToDelete {
		t.Errorf("scratch finding claims SafeToDelete — it is breakdown inside the parent's bytes, never a second claim")
	}
	if scratch[0].Bytes < 4096 {
		t.Errorf("scratch bytes = %d, want ≥4096", scratch[0].Bytes)
	}
}

// TestLookupByEmbeddedProposalID covers the cross-owner join: seal facts
// land on the dispatch scope, settle facts on a parent owner — the
// classifier must resolve the dispatch scope's state from both.
func TestLookupByEmbeddedProposalID(t *testing.T) {
	store := newTestStore(t)
	// Seal on the dispatch scope; settle on the parent, both carrying the
	// proposal id ("proposal:<nanos>:<seq>" — the owner IS the scope id
	// minus the "scope:" prefix).
	appendProposalFact(t, store, "scope:proposal:555:1", dispatch.SchemaProposalSealed, "proposal:555:1")
	appendProposalFact(t, store, "owner:parent:main", dispatch.SchemaProposalSettled, "proposal:555:1")

	index, err := proposalLifecycle(context.Background(), store)
	if err != nil {
		t.Fatalf("proposalLifecycle: %v", err)
	}
	if got := index.stateFor("scope:proposal:555:1"); got != lifecycleSettled {
		t.Errorf("stateFor(dispatch scope) = %v, want settled", got)
	}
	if got := index.idFor("scope:proposal:555:1"); got != "proposal:555:1" {
		t.Errorf("idFor(dispatch scope) = %q, want proposal:555:1", got)
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
