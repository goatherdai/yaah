package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
)

// toAttemptInt reads a payload number whichever way it survived the round trip.
func toAttemptInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return -1
}

// TestEnvelopeAttemptFields pins the backward-compatible enrichment
// (yaah-i5k8): an ordinary single-attempt dispatch reports attempts 1 and no
// retried, and a retried one follows its attempt ids.
func TestEnvelopeAttemptFields(t *testing.T) {
	t.Run("single attempt", func(t *testing.T) {
		env := DispatchResult{
			Status: DispatchCompleted,
			Result: "done",
			Meta:   EnvelopeMeta{DispatchID: "developer#1", Role: "developer", Lane: string(LaneReport)},
		}.Envelope()
		if env.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", env.Attempts)
		}
		if env.Retried {
			t.Error("retried = true, want false for a single attempt")
		}
		if len(env.AttemptIDs) != 1 || env.AttemptIDs[0] != "developer#1" {
			t.Errorf("attempt_ids = %v, want [developer#1]", env.AttemptIDs)
		}
		if env.FailureClass != "" {
			t.Errorf("failure_class = %q, want empty for a completed dispatch", env.FailureClass)
		}
	})

	t.Run("attempt id falls back to the proposal", func(t *testing.T) {
		env := DispatchResult{
			Status:   DispatchProposal,
			Proposal: &proposalEnvelope{Status: EnvelopeStatusProposal, ProposalID: "proposal:x", StopReason: "completed"},
		}.Envelope()
		if env.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", env.Attempts)
		}
		if len(env.AttemptIDs) != 1 || env.AttemptIDs[0] != "proposal:x" {
			t.Errorf("attempt_ids = %v, want [proposal:x]", env.AttemptIDs)
		}
	})

	t.Run("retried envelope", func(t *testing.T) {
		env := proposalEnvelope{Status: EnvelopeStatusError, StopReason: "provider_error", AttemptIDs: []string{"a1", "a2"}}
		env.finalize()
		if env.Attempts != 2 {
			t.Errorf("attempts = %d, want 2", env.Attempts)
		}
		if !env.Retried {
			t.Error("retried = false, want true when attempts > 1")
		}
	})

	t.Run("failure class on the envelope", func(t *testing.T) {
		env := DispatchResult{Status: DispatchTimedOut, Err: context.DeadlineExceeded}.Envelope()
		if env.FailureClass != FailureClassTimeout {
			t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassTimeout)
		}
		if env.StopReason != "timeout" {
			t.Errorf("stop_reason = %q, want timeout", env.StopReason)
		}
	})

	t.Run("lane-supplied class wins", func(t *testing.T) {
		env := DispatchResult{
			Status:       DispatchFailed,
			Err:          context.Canceled,
			FailureClass: FailureClassPreflightTransient,
		}.Envelope()
		if env.FailureClass != FailureClassPreflightTransient {
			t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassPreflightTransient)
		}
	})

	t.Run("sealed partial", func(t *testing.T) {
		env := DispatchResult{
			Status:   DispatchProposal,
			Proposal: &proposalEnvelope{Status: EnvelopeStatusProposal, ProposalID: "proposal:y", StopReason: "turn_budget"},
		}.Envelope()
		if env.FailureClass != FailureClassSealed {
			t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassSealed)
		}
	})
}

// TestRecordDispatchAttemptFact pins that one attempt lands on the root scope's
// owner path with every documented field (yaah-i5k8).
func TestRecordDispatchAttemptFact(t *testing.T) {
	fx, _, rootID := newSessionFixtures(t)
	scope, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatalf("root scope %s not found", rootID)
	}

	recordDispatchAttemptFact(context.Background(), fx.mgr, rootID, attemptEvidenceFor(DispatchResult{
		Status:  DispatchTimedOut,
		Err:     context.DeadlineExceeded,
		Timeout: 0,
		Meta:    EnvelopeMeta{DispatchID: "developer#2"},
	}))

	slice, err := fx.store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, scope.OwnerID(), 100, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	found := false
	for _, id := range slice.FactIDs() {
		rec, ok := slice.FactsByID[id].(kerneltrace.Record)
		if !ok || rec.Envelope.SchemaRef != SchemaDispatchAttempt {
			continue
		}
		found = true
		payload := rec.Body.Payload
		if got := toAttemptInt(payload["attempt"]); got != 1 {
			t.Errorf("attempt = %v, want 1", payload["attempt"])
		}
		if payload["status"] != EnvelopeStatusError {
			t.Errorf("status = %v, want %s", payload["status"], EnvelopeStatusError)
		}
		if payload["retryable"] != true {
			t.Errorf("retryable = %v, want true for a timeout", payload["retryable"])
		}
		if payload["failure_class"] != FailureClassTimeout {
			t.Errorf("failure_class = %v, want %s", payload["failure_class"], FailureClassTimeout)
		}
		if payload["scope_id"] != scope.ID() {
			t.Errorf("scope_id = %v, want %s", payload["scope_id"], scope.ID())
		}
		if payload["trace_owner_id"] != scope.OwnerID() {
			t.Errorf("trace_owner_id = %v, want %s", payload["trace_owner_id"], scope.OwnerID())
		}
		if _, ok := payload["proposal_id"]; !ok {
			t.Error("proposal_id is not recorded")
		}
	}
	if !found {
		t.Fatalf("no %s fact recorded on the root scope's owner path", SchemaDispatchAttempt)
	}

	// A dispatch with no trace wiring is a silent no-op, not a failure.
	recordDispatchAttemptFact(context.Background(), nil, rootID, DispatchAttempt{Attempt: 1})
	recordDispatchAttemptFact(context.Background(), fx.mgr, "", DispatchAttempt{Attempt: 1})
	recordDispatchAttemptFact(context.Background(), fx.mgr, "scope:missing", DispatchAttempt{Attempt: 1})
}

// TestFinishDispatchRecordsAttempt pins the single attempt boundary: a
// foreground dispatch through Run's lanes records exactly one attempt fact, and
// a dispatch with no retained runtime records nothing.
func TestFinishDispatchRecordsAttempt(t *testing.T) {
	fx, _, rootID := newSessionFixtures(t)
	req := DispatchRequest{Retained: &RetainedRuntime{ScopeManager: fx.mgr, RootScopeID: rootID}}
	res := finishDispatch(context.Background(), req, DispatchResult{
		Status: DispatchCompleted,
		Result: "ok",
		Meta:   EnvelopeMeta{DispatchID: "developer#3", Lane: string(LaneReport)},
	})
	if res.Envelope().Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Envelope().Attempts)
	}

	scope, _ := fx.mgr.Get(rootID)
	slice, err := fx.store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, scope.OwnerID(), 100, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	count := 0
	for _, id := range slice.FactIDs() {
		if rec, ok := slice.FactsByID[id].(kerneltrace.Record); ok && rec.Envelope.SchemaRef == SchemaDispatchAttempt {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recorded %d attempt facts, want exactly 1", count)
	}

	// No retained runtime: no owner path to append to, and no error.
	finishDispatch(context.Background(), DispatchRequest{}, DispatchResult{Status: DispatchCompleted})
}
