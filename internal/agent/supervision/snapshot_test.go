package supervision

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// callSeq keeps each appended call's intent ID unique: the store is
// idempotent per intent, so a repeated name+args pair would otherwise be
// deduplicated instead of appended.
var callSeq int

func nextCallSeq() int {
	callSeq++
	return callSeq
}

// newSnapshotStore opens a real trace store for the snapshot tests: the
// builder reads the store seam, so a fake would only re-test the fake.
func newSnapshotStore(t *testing.T) *kerneltrace.SQLiteTraceStore {
	t.Helper()
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open trace store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	return store
}

// appendCall writes one tool call the way the pipeline's PostTool does: a
// declaration, then a capture causally parented on it.
func appendCall(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner, name, args string, success bool, errMsg string) {
	t.Helper()
	ctx := context.Background()
	decl, err := store.Append(ctx, kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:tool:%d", owner, nextCallSeq()),
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: owner,
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Declaration,
				SchemaRef: "yaah.tool." + name + ".v1",
				KindLabel: name,
				Payload:   map[string]any{"tool": name, "args": args},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}
	payload := map[string]any{"tool": name, "success": success}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if _, err := store.Append(ctx, kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:result:%d", owner, nextCallSeq()),
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID:  owner,
			CausalParents: decl.FactIDs,
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Capture,
				SchemaRef: "yaah.tool." + name + ".v1.applied",
				KindLabel: name + ":result",
				Payload:   payload,
			}},
		}},
	}); err != nil {
		t.Fatalf("append capture: %v", err)
	}
}

// registerRun registers one live run with metadata, the way the task runner
// does, and returns the scope ID.
func registerRun(t *testing.T, runs *Registry, owner, role, workdir string) string {
	t.Helper()
	scopeID := "scope:" + owner
	runs.Register(scopeID, Run{
		SteerCh:   make(chan string, SteerBuffer),
		Role:      role,
		Workdir:   workdir,
		StartedAt: time.Now().Add(-90 * time.Second),
	})
	return scopeID
}

// TestSnapshotter_PairsCallsWithCaptures pins the join: declarations pair
// with their captures causally, so the snapshot reports what each call did —
// success, failure reason, and the file a successful write named.
func TestSnapshotter_PairsCallsWithCaptures(t *testing.T) {
	store := newSnapshotStore(t)
	owner := "sub-snap-1"
	appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
	appendCall(t, store, owner, "write", `{"filePath":"b.go","content":"x"}`, true, "")
	appendCall(t, store, owner, "bash", `{"command":"go test"}`, false, "permission denied")

	runs := NewRegistry()
	registerRun(t, runs, owner, "fixer", "/worktrees/sub-1")

	snap := (&Snapshotter{Runs: runs, Store: store}).Snapshot()
	if len(snap.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(snap.Agents))
	}
	a := snap.Agents[0]
	if a.Role != "fixer" || a.Workdir != "/worktrees/sub-1" {
		t.Errorf("metadata = role %q workdir %q, want the registered values", a.Role, a.Workdir)
	}
	if a.OwnerID != owner {
		t.Errorf("OwnerID = %q, want the scope prefix stripped (%q)", a.OwnerID, owner)
	}
	if !a.StillRunning {
		t.Error("a registered run must read as still running")
	}
	if a.Calls != 3 || a.Errors != 1 {
		t.Errorf("calls/errors = %d/%d, want 3/1", a.Calls, a.Errors)
	}
	if a.LastAction != "bash" {
		t.Errorf("LastAction = %q, want the most recent call (bash)", a.LastAction)
	}
	if len(a.Trail) != 3 {
		t.Fatalf("trail = %d calls, want 3", len(a.Trail))
	}
	wantNames := []string{"read", "write", "bash"}
	for i, c := range a.Trail {
		if c.Name != wantNames[i] {
			t.Errorf("trail[%d] = %q, want %q (oldest first)", i, c.Name, wantNames[i])
		}
		if !c.Paired {
			t.Errorf("trail[%d] (%s) not paired with its capture", i, c.Name)
		}
	}
	if got := a.Trail[2]; got.Success || !strings.Contains(got.Error, "permission denied") {
		t.Errorf("failed call digest = %+v, want success=false with its reason", got)
	}
	// The file set is the successful mutating calls' paths: b.go from write,
	// and nothing from read (not mutating) or the failed bash.
	if len(a.ModifiedFiles) != 1 || a.ModifiedFiles[0] != "b.go" {
		t.Errorf("ModifiedFiles = %v, want [b.go]", a.ModifiedFiles)
	}
	if a.Through < 2 {
		t.Errorf("Through = %d, want the highest ordinal observed (>=2: six facts)", a.Through)
	}
	if a.Note != "" {
		t.Errorf("Note = %q, want empty for a clean read", a.Note)
	}
}

// TestSnapshotter_TrailIsBoundedAndTotalsAreNot pins the two-sided bound: the
// trail keeps the LAST 25 calls while the counts report every call observed.
func TestSnapshotter_TrailIsBoundedAndTotalsAreNot(t *testing.T) {
	store := newSnapshotStore(t)
	owner := "sub-snap-long"
	for i := 0; i < 40; i++ {
		appendCall(t, store, owner, "read", `{"filePath":"a.go","n":"`+string(rune('a'+i%26))+`"}`, i%2 == 0, "")
	}
	runs := NewRegistry()
	registerRun(t, runs, owner, "reader", "")

	a := (&Snapshotter{Runs: runs, Store: store}).Snapshot().Agents[0]
	if a.Calls != 40 {
		t.Errorf("Calls = %d, want all 40 observed", a.Calls)
	}
	if len(a.Trail) != maxTrailCalls {
		t.Fatalf("trail = %d, want the %d-call cap", len(a.Trail), maxTrailCalls)
	}
	if a.Errors != 20 {
		t.Errorf("Errors = %d, want the 20 failed calls", a.Errors)
	}
	if len(a.Trail[0].Args) > maxSnapshotArgsBytes+4 {
		t.Errorf("args = %q, want bounded model-authored text", a.Trail[0].Args)
	}
}

// TestSnapshotter_FinishedRunsStayVisible pins the window past dispatch end:
// an unregistered run leaves the live registry but stays readable, which is
// exactly when a supervisor polls "what did that agent just do".
func TestSnapshotter_FinishedRunsStayVisible(t *testing.T) {
	store := newSnapshotStore(t)
	owner := "sub-snap-done"
	appendCall(t, store, owner, "edit", `{"filePath":"c.go"}`, true, "")
	runs := NewRegistry()
	scopeID := registerRun(t, runs, owner, "fixer", "")

	// A live run first: live agents sort ahead of finished ones.
	live := registerRun(t, runs, "sub-snap-live", "live-role", "")
	runs.Unregister(scopeID)

	snap := (&Snapshotter{Runs: runs, Store: store}).Snapshot()
	if len(snap.Agents) != 2 {
		t.Fatalf("agents = %d, want the live run plus the finished one", len(snap.Agents))
	}
	if snap.Agents[0].ScopeID != live || !snap.Agents[0].StillRunning {
		t.Errorf("agents[0] = %+v, want the live run first", snap.Agents[0])
	}
	done := snap.Agents[1]
	if done.ScopeID != scopeID || done.StillRunning {
		t.Fatalf("agents[1] = %+v, want the finished run marked not running", done)
	}
	if done.FinishedAt.IsZero() {
		t.Error("a finished run must carry its FinishedAt")
	}
	if done.Calls != 1 || len(done.ModifiedFiles) != 1 {
		t.Errorf("finished run = calls %d files %v, want its trailing call and file", done.Calls, done.ModifiedFiles)
	}
	// The bridge stays closed: a finished run is not steerable.
	if err := runs.Inject(scopeID, "x"); err == nil {
		t.Error("a finished run must not be injectable")
	}
}

// TestSnapshotter_ReadsAreRepeatable pins the read-only property the
// increment exists for. The type has no handle on an agent (it takes a run
// registry and a trace reader — see Snapshotter), so this asserts the
// observable consequence: polling twice reads the same facts and appends
// nothing, so a snapshot cannot perturb what it summarizes.
func TestSnapshotter_ReadsAreRepeatable(t *testing.T) {
	store := newSnapshotStore(t)
	owner := "sub-snap-stable"
	appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
	runs := NewRegistry()
	registerRun(t, runs, owner, "reader", "")
	s := &Snapshotter{Runs: runs, Store: store, Now: func() time.Time { return time.Unix(1000, 0) }}

	first := s.Snapshot()
	second := s.Snapshot()
	if len(first.Agents) != 1 || len(second.Agents) != 1 {
		t.Fatalf("agents = %d then %d, want 1 each", len(first.Agents), len(second.Agents))
	}
	if first.Agents[0].Calls != second.Agents[0].Calls || first.Agents[0].Errors != second.Agents[0].Errors {
		t.Errorf("counts changed between polls: %d/%d then %d/%d",
			first.Agents[0].Calls, first.Agents[0].Errors, second.Agents[0].Calls, second.Agents[0].Errors)
	}
	if first.At != time.Unix(1000, 0) {
		t.Errorf("At = %v, want the injected clock", first.At)
	}
	if len(second.Agents[0].Trail) != len(first.Agents[0].Trail) {
		t.Errorf("trail length changed between polls: %d then %d",
			len(first.Agents[0].Trail), len(second.Agents[0].Trail))
	}
}

// TestSnapshotter_WithoutStoreOrRuns pins the degraded shapes: no panic, and
// an honest note rather than invented data.
func TestSnapshotter_WithoutStoreOrRuns(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		runs := NewRegistry()
		registerRun(t, runs, "sub-snap-1", "fixer", "")
		snap := (&Snapshotter{Runs: runs}).Snapshot()
		if len(snap.Agents) != 1 {
			t.Fatalf("agents = %d, want the registered run", len(snap.Agents))
		}
		if snap.Agents[0].Note == "" {
			t.Error("a missing trace store must be reported as a note")
		}
		if snap.Agents[0].Through != -1 {
			t.Errorf("Through = %d, want -1 when nothing was read", snap.Agents[0].Through)
		}
	})
	t.Run("no registry", func(t *testing.T) {
		snap := (&Snapshotter{}).Snapshot()
		if len(snap.Agents) != 0 {
			t.Errorf("agents = %d, want none without a registry", len(snap.Agents))
		}
		if snap.At.IsZero() {
			t.Error("At must be stamped even with nothing to report")
		}
	})
	t.Run("not running", func(t *testing.T) {
		var s *Snapshotter
		if snap := s.Snapshot(); len(snap.Agents) != 0 {
			t.Errorf("a nil snapshotter must yield an empty snapshot, got %d agents", len(snap.Agents))
		}
	})
}

// TestSnapshotter_AgentCap pins the batch bound and the overflow count.
func TestSnapshotter_AgentCap(t *testing.T) {
	runs := NewRegistry()
	for i := 0; i < maxSnapshotAgents+3; i++ {
		registerRun(t, runs, "sub-cap-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "r", "")
	}
	snap := (&Snapshotter{Runs: runs}).Snapshot()
	if len(snap.Agents) != maxSnapshotAgents {
		t.Errorf("agents = %d, want the cap %d", len(snap.Agents), maxSnapshotAgents)
	}
	if snap.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", snap.Dropped)
	}
}

// TestSnapshotter_FinishedCapKeepsNewest pins the registry/snapshot cap
// ordering (Qodo review): when live + recently-finished exceed maxSnapshotAgents,
// the newest finished run survives and the oldest are dropped.
func TestSnapshotter_FinishedCapKeepsNewest(t *testing.T) {
	runs := NewRegistry()
	for i := 0; i < 5; i++ {
		registerRun(t, runs, "live-"+string(rune('a'+i)), "r", "")
	}
	for i := 0; i < maxFinishedRuns; i++ {
		owner := "done-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		registerRun(t, runs, owner, "r", "")
		runs.Unregister("scope:" + owner)
	}
	regs := runs.SnapshotEntries()
	if len(regs) != 5+maxFinishedRuns {
		t.Fatalf("SnapshotEntries = %d, want %d", len(regs), 5+maxFinishedRuns)
	}
	for _, live := range regs[:5] {
		if !live.StillRunning {
			t.Errorf("entry %q before finished tail is not live", live.ScopeID)
		}
	}
	// The finished tail must be newest-first: the last registered run is
	// the first finished entry after the live ones.
	if got := regs[5].ScopeID; got != "scope:done-"+string(rune('a'+(maxFinishedRuns-1)%26))+string(rune('a'+(maxFinishedRuns-1)/26)) {
		t.Errorf("first finished entry = %q, want the newest finished run", got)
	}
}

// TestSnapshotSourceIsSatisfied pins the port the tool depends on.
func TestSnapshotSourceIsSatisfied(t *testing.T) {
	var _ supervisionapi.SnapshotSource = (*Snapshotter)(nil)
}

// appendDeclaration writes one tool call's declaration with NO capture —
// the pending-call shape, or the failed-append shape the streak derivation
// must ignore.
func appendDeclaration(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner, name, args string) {
	t.Helper()
	if _, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:tool:%d", owner, nextCallSeq()),
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: owner,
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode:      kerneltrace.Declaration,
				SchemaRef: "yaah.tool." + name + ".v1",
				KindLabel: name,
				Payload:   map[string]any{"tool": name, "args": args},
			}},
		}},
	}); err != nil {
		t.Fatalf("append declaration: %v", err)
	}
}

// TestSnapshotter_ConsecutiveErrors pins the streak derivation (yaah-k0x1):
// paired failures count, a paired success resets, an unpaired declaration
// neither counts nor resets, and the count runs over the FULL fact stream —
// a streak that opened outside the 25-call trail still shows in full.
func TestSnapshotter_ConsecutiveErrors(t *testing.T) {
	snapshotAgent := func(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner string) supervisionapi.AgentSnapshot {
		t.Helper()
		runs := NewRegistry()
		registerRun(t, runs, owner, "builder", "")
		agents := (&Snapshotter{Runs: runs, Store: store}).Snapshot().Agents
		if len(agents) != 1 {
			t.Fatalf("agents = %d, want 1", len(agents))
		}
		return agents[0]
	}

	t.Run("success resets the streak", func(t *testing.T) {
		store := newSnapshotStore(t)
		owner := "sub-streak-reset"
		appendCall(t, store, owner, "bash", `{"command":"x"}`, false, "boom")
		appendCall(t, store, owner, "bash", `{"command":"x"}`, false, "boom")
		appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
		a := snapshotAgent(t, store, owner)
		if a.ConsecutiveErrors != 0 {
			t.Errorf("ConsecutiveErrors = %d, want 0 after the paired success", a.ConsecutiveErrors)
		}
		if a.Errors != 2 {
			t.Errorf("Errors = %d, want the 2 failures", a.Errors)
		}
	})

	t.Run("failures count and an unpaired call neither counts nor resets", func(t *testing.T) {
		store := newSnapshotStore(t)
		owner := "sub-streak-unpaired"
		appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
		appendCall(t, store, owner, "bash", `{"command":"x"}`, false, "boom")
		appendDeclaration(t, store, owner, "write", `{"filePath":"b.go"}`)
		a := snapshotAgent(t, store, owner)
		if a.Calls != 2 {
			t.Errorf("Calls = %d, want the 2 paired calls", a.Calls)
		}
		if a.ConsecutiveErrors != 1 {
			t.Errorf("ConsecutiveErrors = %d, want 1 — the unpaired declaration is a pending call, not an observed failure", a.ConsecutiveErrors)
		}
	})

	t.Run("a streak longer than the trail still counts in full", func(t *testing.T) {
		store := newSnapshotStore(t)
		owner := "sub-streak-long"
		for i := 0; i < 10; i++ {
			appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
		}
		for i := 0; i < 30; i++ {
			appendCall(t, store, owner, "bash", `{"command":"x"}`, false, "boom")
		}
		a := snapshotAgent(t, store, owner)
		if a.Calls != 40 || a.Errors != 30 {
			t.Fatalf("calls/errors = %d/%d, want 40/30", a.Calls, a.Errors)
		}
		if a.ConsecutiveErrors != 30 {
			t.Errorf("ConsecutiveErrors = %d, want the full 30-call streak even though the trail holds 25", a.ConsecutiveErrors)
		}
		if len(a.Trail) != maxTrailCalls {
			t.Errorf("trail = %d, want the cap", len(a.Trail))
		}
	})
}

// TestSnapshotter_Feature pins the feature description (yaah-k0x1): the
// label wins when one was supplied, otherwise the prompt is trimmed,
// whitespace-collapsed, and clipped to MaxFeatureRunes runes — and the
// builder passes it through, clipping defensively.
func TestSnapshotter_Feature(t *testing.T) {
	t.Run("label wins over the prompt", func(t *testing.T) {
		if got := DeriveFeature("ship the fix", "do\nthe\tother   thing"); got != "ship the fix" {
			t.Errorf("DeriveFeature = %q, want the label", got)
		}
	})
	t.Run("prompt is trimmed and whitespace-collapsed", func(t *testing.T) {
		if got := DeriveFeature("", "  fix   the\n\nflaky\ttest  "); got != "fix the flaky test" {
			t.Errorf("DeriveFeature = %q, want the collapsed prompt", got)
		}
	})
	t.Run("clipped to MaxFeatureRunes runes at a multi-byte boundary", func(t *testing.T) {
		got := DeriveFeature("", strings.Repeat("é", 500)) // 2 bytes per rune
		if n := len([]rune(got)); n != supervisionapi.MaxFeatureRunes {
			t.Fatalf("clipped length = %d runes, want %d", n, supervisionapi.MaxFeatureRunes)
		}
		// The clip slices and adds nothing — the reference implementation
		// appends no marker, so the tail is the cap-th rune, not an ellipsis.
		if got != strings.Repeat("é", supervisionapi.MaxFeatureRunes) {
			t.Errorf("clip must slice to the cap and add nothing, got a %d-rune string", len([]rune(got)))
		}
		exact := DeriveFeature("", strings.Repeat("é", supervisionapi.MaxFeatureRunes))
		if len([]rune(exact)) != supervisionapi.MaxFeatureRunes {
			t.Errorf("an exactly-max string must pass through unchanged, got %d runes", len([]rune(exact)))
		}
	})
	t.Run("the builder passes the derived feature through", func(t *testing.T) {
		store := newSnapshotStore(t)
		owner := "sub-feature"
		appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
		runs := NewRegistry()
		runs.Register("scope:"+owner, Run{
			SteerCh:   make(chan string, SteerBuffer),
			Role:      "builder",
			StartedAt: time.Now().Add(-time.Minute),
			Feature:   DeriveFeature("", "repair the\n   flaky  test"),
		})
		a := (&Snapshotter{Runs: runs, Store: store}).Snapshot().Agents[0]
		if a.Feature != "repair the flaky test" {
			t.Errorf("Feature = %q, want the derived description", a.Feature)
		}
	})
	t.Run("the builder clips defensively", func(t *testing.T) {
		runs := NewRegistry()
		runs.Register("scope:sub-feature-clipped", Run{
			SteerCh: make(chan string, SteerBuffer),
			Feature: strings.Repeat("é", supervisionapi.MaxFeatureRunes*2),
		})
		a := (&Snapshotter{Runs: runs}).Snapshot().Agents[0]
		if n := len([]rune(a.Feature)); n != supervisionapi.MaxFeatureRunes {
			t.Errorf("Feature = %d runes, want the defensive clip to %d", n, supervisionapi.MaxFeatureRunes)
		}
	})
	t.Run("empty stays empty", func(t *testing.T) {
		runs := NewRegistry()
		registerRun(t, runs, "sub-feature-empty", "builder", "")
		a := (&Snapshotter{Runs: runs}).Snapshot().Agents[0]
		if a.Feature != "" {
			t.Errorf("Feature = %q, want empty when the registrant supplied none", a.Feature)
		}
	})
}

// TestSnapshotter_RegistrationFlagsPassThrough pins the reversibility
// flags (yaah-k0x1): what the registration carries is what the snapshot
// reports, Reversible arrives only through the registry's mark — it has
// no registration input — a root-style registration with neither flag
// reports false/false, and a finished run keeps the flags it had.
func TestSnapshotter_RegistrationFlagsPassThrough(t *testing.T) {
	t.Run("flags pass through", func(t *testing.T) {
		runs := NewRegistry()
		runs.Register("scope:flags", Run{
			SteerCh:      make(chan string, SteerBuffer),
			Redirectable: true,
		})
		gen, ok := runs.Generation("scope:flags")
		if !ok {
			t.Fatal("Generation must report the live registration")
		}
		runs.MarkReversible("scope:flags", gen)
		a := (&Snapshotter{Runs: runs}).Snapshot().Agents[0]
		if !a.Reversible || !a.Redirectable {
			t.Errorf("flags = %v/%v, want the marked reversible and the registered redirectable", a.Reversible, a.Redirectable)
		}
	})
	t.Run("root-style registration reports false/false", func(t *testing.T) {
		runs := NewRegistry()
		registerRun(t, runs, "sub-rootish", "root", "")
		a := (&Snapshotter{Runs: runs}).Snapshot().Agents[0]
		if a.Reversible || a.Redirectable {
			t.Errorf("flags = %v/%v, want false/false for a registration that set neither", a.Reversible, a.Redirectable)
		}
	})
	t.Run("a finished run keeps its flags", func(t *testing.T) {
		runs := NewRegistry()
		runs.Register("scope:done-flags", Run{
			SteerCh: make(chan string, SteerBuffer),
		})
		gen, ok := runs.Generation("scope:done-flags")
		if !ok {
			t.Fatal("Generation must report the live registration")
		}
		runs.MarkReversible("scope:done-flags", gen)
		runs.Unregister("scope:done-flags")
		a := (&Snapshotter{Runs: runs}).Snapshot().Agents[0]
		if a.StillRunning {
			t.Fatal("the run must read as finished")
		}
		if !a.Reversible || a.Redirectable {
			t.Errorf("flags = %v/%v, want the finished run's marked reversible", a.Reversible, a.Redirectable)
		}
	})
}

// TestSnapshotter_InFlight pins the in-flight feed (yaah-k0x1): a started
// call appears with its age at the snapshot's clock, disappears on return,
// and a scope that is no longer running shows none — a call cut off by
// cancellation has no end event, and it vanishes with its registration.
func TestSnapshotter_InFlight(t *testing.T) {
	store := newSnapshotStore(t)
	owner := "sub-inflight"
	appendCall(t, store, owner, "read", `{"filePath":"a.go"}`, true, "")
	runs := NewRegistry()
	scopeID := registerRun(t, runs, owner, "builder", "")
	gen, ok := runs.Generation(scopeID)
	if !ok {
		t.Fatal("Generation must report the live registration")
	}
	tbl := runs.InFlightTable(scopeID, gen)
	if tbl == nil {
		t.Fatal("the registered run must expose its in-flight table")
	}

	start := time.Unix(1000, 0)
	tbl.Start(1, "bash", `{"command":"go test ./..."}`, start)
	tbl.Start(2, "edit", strings.Repeat("x", maxSnapshotArgsBytes+80), start.Add(time.Second))

	s := &Snapshotter{Runs: runs, Store: store, Now: func() time.Time { return start.Add(2 * time.Second) }}
	a := s.Snapshot().Agents[0]
	if len(a.InFlight) != 2 {
		t.Fatalf("InFlight = %d calls, want the two open calls", len(a.InFlight))
	}
	if a.InFlight[0].Name != "bash" || a.InFlight[0].Age != 2*time.Second {
		t.Errorf("InFlight[0] = {%s, %v}, want bash aged 2s (oldest first)", a.InFlight[0].Name, a.InFlight[0].Age)
	}
	if a.InFlight[1].Name != "edit" || a.InFlight[1].Age != 1*time.Second {
		t.Errorf("InFlight[1] = {%s, %v}, want edit aged 1s", a.InFlight[1].Name, a.InFlight[1].Age)
	}
	if len(a.InFlight[1].Args) > maxSnapshotArgsBytes+3 || !strings.HasSuffix(a.InFlight[1].Args, "…") {
		t.Errorf("InFlight[1].Args = %d bytes, want the args cap with an ellipsis tail", len(a.InFlight[1].Args))
	}

	// On return the call disappears.
	tbl.End(1)
	tbl.End(2)
	if a := s.Snapshot().Agents[0]; a.InFlight != nil {
		t.Errorf("InFlight after both calls returned = %+v, want nil", a.InFlight)
	}

	// A finished scope shows none even with a call still open: the
	// cancellation cut the call off, and the table went with the
	// registration.
	tbl.Start(3, "write", `{"filePath":"b.go"}`, start)
	runs.Unregister(scopeID)
	a = s.Snapshot().Agents[0]
	if a.StillRunning {
		t.Fatal("the unregistered run must read as finished")
	}
	if a.InFlight != nil {
		t.Errorf("a finished scope must show no in-flight calls, got %+v", a.InFlight)
	}
}
