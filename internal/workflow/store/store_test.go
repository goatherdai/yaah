package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/memory"
	"github.com/goatherdai/yaah/internal/workflow/exec"
	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/trace"
)

// testStore opens a fresh SQLite store in a temp directory. Store.Close is the
// caller's job in production (an open file cannot be removed on Windows), so
// the test registers the close alongside the temp directory.
func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := memory.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

// richCheckpoint is a checkpoint carrying every projection the adapter writes:
// run row, node payloads with an attempt and a proposal, settlement and
// fan-out ledgers, a body instance, an event tail, and typed trace records.
// It deliberately includes the structured call outcome so a regression that
// drops those fields from the payload cannot pass this test.
func richCheckpoint() exec.Checkpoint {
	return exec.Checkpoint{
		RunID:        "run-1",
		SpecHash:     "abc123",
		SpecPath:     "flow.yaml",
		GraphName:    "flow",
		Status:       exec.RunRunning,
		Dispatches:   3,
		PeakInFlight: 2,
		StartedAt:    time.Unix(1, 0).UTC(),
		Params:       map[string]any{"version": "2.4", "dry_run": false, "ratio": 1.5},
		Ledger: exec.Ledger{
			Open:       map[string]string{"p1": "steps.build"},
			Claims:     map[string]string{"p2": "steps.review"},
			Settled:    map[string]string{"p3": "steps.land"},
			AbortAsked: map[string]string{"p4": "discard"},
		},
		Seq:      4,
		Warnings: []string{"a declined artifact"},
		Nodes: []*exec.NodeState{
			{
				Path: "steps.build", ID: "build", Kind: ir.KindDelegate,
				State: exec.StateSucceeded, Needs: []string{"plan"},
				Policy: ir.OnFailureContinue, MaxAttempts: 2,
				Attempts: []*exec.Attempt{{
					Number: 1, NodePath: "steps.build", DispatchID: "steps.build#1",
					Handle:           exec.Handle{DispatchID: "steps.build#1", Ref: "job-1"},
					Role:             "developer",
					Lane:             "proposal",
					StartedAt:        time.Unix(2, 0).UTC(),
					FinishedAt:       time.Unix(3, 0).UTC(),
					Status:           "proposal",
					StopReason:       "completed",
					DurationMs:       1000,
					Model:            "test-model",
					World:            "worktree",
					ProposalID:       "p1",
					Result:           "done",
					RecoveryAsserted: true,
				}},
				Status: "proposal", StopReason: "completed", DurationMs: 1000,
				Model: "test-model", World: "worktree", ProposalID: "p1", Result: "done",
				Outputs: []exec.NodeOutput{{
					NodePath: "steps.build", Kind: ir.KindDelegate, Name: "report", Value: "done",
				}},
				Proposal: &exec.ProposalHandle{
					ProposalID: "p1", Producer: "steps.build", Role: "developer",
					Lane: "proposal", DispatchID: "steps.build#1",
				},
				Settled: []exec.Settled{{
					ProposalID: "p1", Producer: "steps.build", Action: "apply",
					Status: "settled", Files: []string{"src/a.go"},
				}},
				Items:   []exec.FanoutItem{{Index: 0, Bound: "a.go", Paths: []string{"steps.build.body[0].fix"}, State: exec.StateSucceeded}},
				Winners: []int{0},
				Collect: []any{"a.go"},
			},
			{
				Path: "steps.sub", Kind: ir.KindCall, State: exec.StateFailed,
				Reason:          "call_failed: subworkflow helper (run run-1!steps.sub) — boom",
				CallRunID:       "run-1!steps.sub",
				CallChildStatus: exec.RunFailed,
				CallChildReason: "boom",
			},
		},
		Instances: []exec.InstanceState{{
			Path: "steps.fan.body[0].review", Owner: "steps.fan",
			BodyPath: "steps.fan.body[0]", Item: 0, Name: "file", Bound: "a.go",
			Locals: map[string]string{"review": "steps.fan.body[0].review"},
		}},
		Events: []string{"1: run_started flow", "2: dispatch_started steps.build#1"},
		Trace: []trace.Record{
			{Seq: 1, EventSeq: 1, Kind: trace.KindRunStarted, RunID: "run-1", SpecHash: "abc123", Status: trace.StatusStarted},
			{Seq: 2, EventSeq: 2, Kind: trace.KindDispatch, RunID: "run-1", NodePath: "steps.build",
				DispatchID: "steps.build#1", Attempt: 1, Status: "proposal", Result: "done"},
		},
	}
}

func jsonOf[T any](t *testing.T, v T) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// TestSaveLoadCheckpointRoundTrip proves the SQLite adapter projects every
// fact of a checkpoint without losing or inventing one. Node, instance, and
// trace payloads are compared as JSON because the store itself round-trips
// them through JSON; a byte-equal re-marshal means every field survived.
func TestSaveLoadCheckpointRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	want := richCheckpoint()
	if err := st.SaveCheckpoint(ctx, want); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	got, err := st.LoadCheckpoint(ctx, want.RunID)
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}

	if got.RunID != want.RunID || got.SpecHash != want.SpecHash || got.SpecPath != want.SpecPath || got.GraphName != want.GraphName {
		t.Errorf("identity = %s/%s/%s/%s, want %s/%s/%s/%s",
			got.RunID, got.SpecHash, got.SpecPath, got.GraphName,
			want.RunID, want.SpecHash, want.SpecPath, want.GraphName)
	}
	if got.Status != want.Status || got.Dispatches != want.Dispatches || got.PeakInFlight != want.PeakInFlight || got.Seq != want.Seq {
		t.Errorf("tallies = %s/%d/%d/%d, want %s/%d/%d/%d",
			got.Status, got.Dispatches, got.PeakInFlight, got.Seq,
			want.Status, want.Dispatches, want.PeakInFlight, want.Seq)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) {
		t.Errorf("times = %v/%v, want %v/%v", got.StartedAt, got.FinishedAt, want.StartedAt, want.FinishedAt)
	}
	if jsonOf(t, got.Params) != jsonOf(t, want.Params) {
		t.Errorf("params = %s, want %s", jsonOf(t, got.Params), jsonOf(t, want.Params))
	}
	if !reflect.DeepEqual(got.Ledger, want.Ledger) {
		t.Errorf("ledger = %+v, want %+v", got.Ledger, want.Ledger)
	}
	if !slices.Equal(got.Warnings, want.Warnings) {
		t.Errorf("warnings = %v, want %v", got.Warnings, want.Warnings)
	}
	if !slices.Equal(got.Events, want.Events) {
		t.Errorf("events = %v, want %v", got.Events, want.Events)
	}
	if got.EventBase != len(want.Events) || got.TraceBase != len(want.Trace) {
		t.Errorf("bases = %d/%d, want %d/%d", got.EventBase, got.TraceBase, len(want.Events), len(want.Trace))
	}
	if a, b := jsonOf(t, got.Nodes), jsonOf(t, want.Nodes); a != b {
		t.Errorf("nodes round-trip changed:\n got %s\nwant %s", a, b)
	}
	if a, b := jsonOf(t, got.Instances), jsonOf(t, want.Instances); a != b {
		t.Errorf("instances round-trip changed:\n got %s\nwant %s", a, b)
	}
	if a, b := jsonOf(t, got.Trace), jsonOf(t, want.Trace); a != b {
		t.Errorf("trace round-trip changed:\n got %s\nwant %s", a, b)
	}
}

// TestSaveCheckpointIsIdempotent pins the adapter's central claim: every write
// is an upsert, so re-saving the same projection twice leaves no duplicate
// nodes, instances, events, or trace records. LoadCheckpoint appends every row
// it finds, so a duplicate would show up in the counts.
func TestSaveCheckpointIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	cp := richCheckpoint()
	if err := st.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	first, err := st.LoadCheckpoint(ctx, cp.RunID)
	if err != nil {
		t.Fatalf("LoadCheckpoint (first): %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := st.SaveCheckpoint(ctx, cp); err != nil {
			t.Fatalf("SaveCheckpoint (repeat %d): %v", i+1, err)
		}
	}
	again, err := st.LoadCheckpoint(ctx, cp.RunID)
	if err != nil {
		t.Fatalf("LoadCheckpoint (after repeats): %v", err)
	}
	if len(again.Nodes) != len(first.Nodes) || len(again.Instances) != len(first.Instances) ||
		len(again.Events) != len(first.Events) || len(again.Trace) != len(first.Trace) {
		t.Errorf("re-save changed row counts: nodes %d→%d, instances %d→%d, events %d→%d, trace %d→%d",
			len(first.Nodes), len(again.Nodes), len(first.Instances), len(again.Instances),
			len(first.Events), len(again.Events), len(first.Trace), len(again.Trace))
	}
	if jsonOf(t, again.Nodes) != jsonOf(t, first.Nodes) {
		t.Error("re-save changed a node payload")
	}
}

// TestCheckpointAppendsNewEvents models the reducer's high-water-mark pattern:
// a later checkpoint carries EventBase/TraceBase plus only the NEW lines, and
// the store must append them to what is already recorded rather than replace
// or duplicate the tail.
func TestCheckpointAppendsNewEvents(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	first := richCheckpoint()
	if err := st.SaveCheckpoint(ctx, first); err != nil {
		t.Fatalf("SaveCheckpoint (first): %v", err)
	}
	loaded, err := st.LoadCheckpoint(ctx, first.RunID)
	if err != nil {
		t.Fatalf("LoadCheckpoint (first): %v", err)
	}

	next := richCheckpoint()
	next.EventBase = len(loaded.Events)
	next.Events = []string{"3: node_succeeded steps.build"}
	next.TraceBase = len(loaded.Trace)
	next.Trace = []trace.Record{{
		Seq: 3, EventSeq: 3, Kind: trace.KindNodeFinished, RunID: next.RunID,
		NodePath: "steps.build", Status: trace.StatusSucceeded,
	}}
	if err := st.SaveCheckpoint(ctx, next); err != nil {
		t.Fatalf("SaveCheckpoint (append): %v", err)
	}
	got, err := st.LoadCheckpoint(ctx, first.RunID)
	if err != nil {
		t.Fatalf("LoadCheckpoint (append): %v", err)
	}
	wantEvents := append(append([]string(nil), loaded.Events...), next.Events...)
	if !slices.Equal(got.Events, wantEvents) {
		t.Errorf("events = %v, want %v", got.Events, wantEvents)
	}
	if len(got.Trace) != len(loaded.Trace)+1 || got.Trace[len(got.Trace)-1].Seq != 3 {
		t.Errorf("trace = %+v, want the original records plus seq 3", got.Trace)
	}
}

func TestLoadCheckpointMissingRun(t *testing.T) {
	st := testStore(t)
	_, err := st.LoadCheckpoint(t.Context(), "no-such-run")
	if !errors.Is(err, exec.ErrRunNotFound) {
		t.Errorf("LoadCheckpoint error = %v, want ErrRunNotFound", err)
	}
}
