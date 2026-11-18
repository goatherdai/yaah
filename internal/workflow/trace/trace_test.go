package trace

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TestKindsPinsTheVocabulary guards the closed record vocabulary: seven kinds,
// one order, no duplicates. The trace surface is a contract for readers, so a
// kind cannot be added or reordered without this test saying so.
func TestKindsPinsTheVocabulary(t *testing.T) {
	want := []Kind{
		KindRunStarted,
		KindNodeStarted,
		KindDispatch,
		KindProposal,
		KindSettle,
		KindNodeFinished,
		KindRunEnded,
	}
	got := Kinds()
	if !slices.Equal(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
	seen := map[Kind]bool{}
	for _, k := range got {
		if seen[k] {
			t.Errorf("Kinds() repeats %q", k)
		}
		seen[k] = true
	}
}

func TestDigest(t *testing.T) {
	if got := Digest(""); got != "" {
		t.Errorf("Digest(\"\") = %q, want empty (no result is not the digest of nothing)", got)
	}
	// SHA-256 of "hello", pinned so a change in the digest algorithm is a
	// deliberate contract change, not an accident of a refactor.
	const hello = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := Digest("hello"); got != hello {
		t.Errorf("Digest(\"hello\") = %q, want %q", got, hello)
	}
}

// TestRecordJSONRoundTrip pins that every fact a record can carry survives the
// JSON projection the checkpoint store writes. Retryable is a pointer so
// "asserted false" stays distinct from "asserted nothing"; Files and
// ChildRunID are the replay-only fields most easily dropped by a refactor.
func TestRecordJSONRoundTrip(t *testing.T) {
	retryable := false
	want := Record{
		Seq: 7, EventSeq: 6, Kind: KindDispatch, RunID: "run-1", SpecHash: "abc123",
		NodePath: "steps.x", NodeID: "x", NodeKind: "delegate", ScopeID: "steps.fan",
		Lane: "proposal", Attempt: 2, DispatchID: "steps.x#2",
		Status: "error", Reason: "why", ProposalID: "p1", Action: "apply",
		Result: "res", OutputDigest: Digest("res"), Error: "boom",
		Retryable: &retryable, ChildRunID: "run-1!steps.sub", Files: []string{"a", "b"},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record round-trip changed:\n got %+v\nwant %+v", got, want)
	}
}
