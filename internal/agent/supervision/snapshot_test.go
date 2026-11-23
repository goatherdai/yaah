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
