package yaah

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	"github.com/goatherdai/yaah/internal/dispatch"
	"github.com/goatherdai/yaah/internal/memory"
)

func TestTraceConflictsAggregate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	dir := filepath.Join(home, "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setTestTraceConfig(t, home, dir)

	path := filepath.Join(dir, "trace.sqlite")
	store, err := kerneltrace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	appendFact := func(owner, schema, kind string, mode kerneltrace.RecordMode, payload map[string]any) {
		t.Helper()
		_, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
			AppendIntentID: owner + ":" + schema + ":" + kind,
			Groups: []kerneltrace.AppendGroup{{
				TraceOwnerID: owner,
				FactDrafts: []kerneltrace.RecordDraft{{
					Mode:      mode,
					SchemaRef: schema,
					KindLabel: kind,
					Payload:   payload,
				}},
			}},
		})
		if err != nil {
			t.Fatalf("append %s: %v", schema, err)
		}
	}

	appendFact("sub-a1", "yaah.subagent.usage.v1", "subagent:usage", kerneltrace.Capture, map[string]any{"lane": "retained", "parent_session": "sess-a"})
	appendFact("sub-a2", "yaah.subagent.usage.v1", "subagent:usage:2", kerneltrace.Capture, map[string]any{"lane": "shared", "parent_session": "sess-a"})
	appendFact("sess-a", dispatch.SchemaProposalSealed, "proposal", kerneltrace.Declaration, map[string]any{"proposal_id": "p1"})
	appendFact("sub-a1", "goatherd.merge.proposed.v1", "merge:proposed", kerneltrace.Declaration, map[string]any{"proposal_id": "p1"})
	appendFact("sess-a", dispatch.SchemaProposalConflict, "conflict", kerneltrace.Declaration, map[string]any{
		"kind": "same_path_divergent", "paths": []string{"a.txt", "b.txt"}, "proposals": []string{"p1", "p2"},
	})
	appendFact("sub-b1", "yaah.subagent.usage.v1", "subagent:usage", kerneltrace.Capture, map[string]any{"lane": "shared", "parent_session": "sess-b"})
	db, err := memory.OpenDefault()
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	if err := db.RecordTraceOwner("sub-old", "sess-a", "worker"); err != nil {
		t.Fatalf("record legacy owner: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close state db: %v", err)
	}
	appendFact("sub-old", "yaah.subagent.usage.v1", "subagent:usage:old", kerneltrace.Capture, map[string]any{"lane": "shared"})
	appendFact("sub-unmapped", "yaah.subagent.usage.v1", "subagent:usage:unmapped", kerneltrace.Capture, map[string]any{"lane": "shared"})
	appendFact("sess-b", dispatch.SchemaProposalConflict, "conflict", kerneltrace.Declaration, map[string]any{
		"kind": "delete_modify", "paths": []string{"a.txt"}, "proposals": []string{"p3", "p4"},
	})
	store.Close(context.Background())

	var buf bytes.Buffer
	cmd := newGoatherdTraceConflictsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var got conflictAggregate
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, buf.String())
	}
	if got.Sessions != 2 || got.Conflicts != 2 || got.ConflictSessions != 2 {
		t.Errorf("sessions/conflicts = %d/%d/%d, want 2/2/2 (report %+v)", got.Sessions, got.Conflicts, got.ConflictSessions, got)
	}
	if got.Dispatches["retained"] != 1 || got.Dispatches["shared"] != 3 {
		t.Errorf("dispatches = %+v, want retained=1 shared=3 (the unmapped owner counts under unresolved, not a lane)", got.Dispatches)
	}
	if got.UnresolvedDispatches != 1 {
		t.Errorf("unresolved dispatches = %d, want 1", got.UnresolvedDispatches)
	}
	if got.SealedProposals != 2 || got.ConflictProposals != 4 {
		t.Errorf("sealed/proposals = %d/%d, want 2/4", got.SealedProposals, got.ConflictProposals)
	}
	if got.FanoutSessions != 1 || got.FanoutSessionsWithConflicts != 1 {
		t.Errorf("fanout = %d/%d, want 1/1", got.FanoutSessions, got.FanoutSessionsWithConflicts)
	}
	if len(got.RepeatedPaths) != 1 || got.RepeatedPaths[0].Path != "a.txt" || got.RepeatedPaths[0].Count != 2 {
		t.Errorf("repeated paths = %+v, want a.txt×2", got.RepeatedPaths)
	}
	if got.Kinds["same_path_divergent"] != 1 || got.Kinds["delete_modify"] != 1 {
		t.Errorf("kinds = %+v, want both conflict kinds", got.Kinds)
	}
}

func TestTraceConflictsSessionWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	dir := filepath.Join(home, "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setTestTraceConfig(t, home, dir)
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(dir, "trace.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
		AppendIntentID: "one",
		Groups: []kerneltrace.AppendGroup{{
			TraceOwnerID: "sub-only",
			FactDrafts: []kerneltrace.RecordDraft{{
				Mode: kerneltrace.Capture, SchemaRef: "yaah.subagent.usage.v1", KindLabel: "subagent:usage",
				Payload: map[string]any{"lane": "retained", "parent_session": "sess-only"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close(context.Background())

	var buf bytes.Buffer
	cmd := newGoatherdTraceConflictsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--session", "sess-only"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("session sess-only")) || !bytes.Contains(buf.Bytes(), []byte("dispatches")) {
		t.Errorf("windowed report = %s", buf.String())
	}
}

// TestTraceConflictsRedirectedDispatchNotFanout pins PR #400 review
// finding 4: usage facts are per GENERATION now, so one owner carries a
// fact per generation of the same dispatch. A redirected dispatch must
// still count as ONE dispatch — never a manufactured second dispatch
// that turns a single-dispatch session into a fan-out session.
func TestTraceConflictsRedirectedDispatchNotFanout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	dir := filepath.Join(home, "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setTestTraceConfig(t, home, dir)

	path := filepath.Join(dir, "trace.sqlite")
	store, err := kerneltrace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close(context.Background())

	// One redirected dispatch: two generations, two usage facts, one owner.
	for _, gen := range []string{"g1", "g2"} {
		_, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
			AppendIntentID: "sub-r1:usage:" + gen,
			Groups: []kerneltrace.AppendGroup{{
				TraceOwnerID: "sub-r1",
				FactDrafts: []kerneltrace.RecordDraft{{
					Mode:      kerneltrace.Capture,
					SchemaRef: "yaah.subagent.usage.v1",
					KindLabel: "subagent:usage",
					Payload: map[string]any{
						"lane": "retained", "parent_session": "sess-r",
					},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("append usage %s: %v", gen, err)
		}
	}

	var buf bytes.Buffer
	cmd := newGoatherdTraceConflictsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got conflictAggregate
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, buf.String())
	}
	if got.Dispatches["retained"] != 1 {
		t.Errorf("dispatches = %+v, want retained=1 (two facts, one dispatch)", got.Dispatches)
	}
	if got.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", got.Sessions)
	}
	if got.FanoutSessions != 0 || got.FanoutSessionsWithConflicts != 0 {
		t.Errorf("fanout = %d/%d, want 0/0 — a redirect is not a fan-out", got.FanoutSessions, got.FanoutSessionsWithConflicts)
	}
}

// TestTraceConflictsSessionFilterCountsOnlyRequestedSession pins the
// qodo follow-up on PR #400: in a --session-filtered report the lane
// totals must count only the requested session's dispatches — always,
// including the per-owner pass. Two sessions with disjoint lanes must
// each appear alone in their own window.
func TestTraceConflictsSessionFilterCountsOnlyRequestedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	dir := filepath.Join(home, "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	setTestTraceConfig(t, home, dir)

	path := filepath.Join(dir, "trace.sqlite")
	store, err := kerneltrace.NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close(context.Background())

	appendUsage := func(owner, parent, lane, intent string) {
		t.Helper()
		_, err := store.Append(context.Background(), kerneltrace.TrustedAppendContext, kerneltrace.AppendBatch{
			AppendIntentID: intent,
			Groups: []kerneltrace.AppendGroup{{
				TraceOwnerID: owner,
				FactDrafts: []kerneltrace.RecordDraft{{
					Mode:      kerneltrace.Capture,
					SchemaRef: "yaah.subagent.usage.v1",
					KindLabel: "subagent:usage",
					Payload:   map[string]any{"lane": lane, "parent_session": parent},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("append usage for %s: %v", owner, err)
		}
	}
	appendUsage("sub-a1", "sess-a", "retained", "sub-a1:usage:g1")
	appendUsage("sub-a2", "sess-a", "retained", "sub-a2:usage:g1")
	appendUsage("sub-b1", "sess-b", "shared", "sub-b1:usage:g1")
	appendUsage("sub-b2", "sess-b", "shared", "sub-b2:usage:g2")

	run := func(args ...string) conflictAggregate {
		t.Helper()
		var buf bytes.Buffer
		cmd := newGoatherdTraceConflictsCmd()
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v): %v", args, err)
		}
		var got conflictAggregate
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("report is not JSON: %v\n%s", err, buf.String())
		}
		return got
	}

	a := run("--json", "--session", "sess-a")
	if a.Dispatches["retained"] != 2 || a.Dispatches["shared"] != 0 {
		t.Errorf("sess-a dispatches = %+v, want retained=2 shared=0 (no sess-b leakage)", a.Dispatches)
	}
	if a.Sessions != 1 {
		t.Errorf("sess-a sessions = %d, want 1", a.Sessions)
	}

	b := run("--json", "--session", "sess-b")
	if b.Dispatches["shared"] != 2 || b.Dispatches["retained"] != 0 {
		t.Errorf("sess-b dispatches = %+v, want shared=2 retained=0 (no sess-a leakage)", b.Dispatches)
	}
	if b.Sessions != 1 {
		t.Errorf("sess-b sessions = %d, want 1", b.Sessions)
	}
}
