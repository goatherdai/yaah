package dispatch

import (
	"context"
	"log/slog"

	goatherd "github.com/goatherdai/goatherd-runtime"
)

// attempt_evidence.go records one trace fact per dispatch attempt
// (yaah-i5k8, decision yaah-hxk3.5). A provider error and a stuck child used to
// be legible only through the run's free-form error text; the attempt fact makes
// the dispatch-level verdict itself the evidence: which attempt it was, how it
// ended, whether the retry policy may run it again, and the identities a trace
// reader correlates it with.
//
// It follows the dispatch lane's existing fact convention — the one
// yaah.subagent.world.v1 uses in retained.go: a best-effort AppendFact on a
// kernel scope, never a reason to fail a dispatch that already ran. The fact
// lands on the session's ROOT scope rather than the attempt's child world,
// because not every lane creates a child world (a shared dispatch, a preflight
// failure, a background accept) and one owner path is what makes the attempt
// history readable in order. The root is also the owner path a trace reader
// already walks: it is the scope whose OwnerID() is the session id.

// SchemaDispatchAttempt is the trace schema ref for one dispatch attempt.
const SchemaDispatchAttempt = "yaah.subagent.dispatch_attempt.v1"

// dispatchAttemptKind is the record's human-facing kind label.
const dispatchAttemptKind = "dispatch_attempt"

// DispatchAttempt is one dispatch attempt's evidence, exactly as
// yaah.subagent.dispatch_attempt.v1 stores it. Attempt is 1-based; with no
// retry policy in force every dispatch reports attempt 1.
type DispatchAttempt struct {
	// Attempt is the 1-based attempt number within one dispatch.
	Attempt int
	// Status is the envelope status the attempt reported: proposal, report,
	// shared, running, completed, or error.
	Status string
	// Retryable reports that the retry policy may re-run this attempt. It is
	// derived from FailureClass (the first four classes are retry
	// candidates), not from ResultRecovery.Retryable.
	Retryable bool
	// FailureClass is the dispatch-level failure classification, or "" when
	// the attempt did not fail.
	FailureClass string
	// ScopeID is the kernel scope the fact was appended to.
	ScopeID string
	// TraceOwnerID is that scope's owner — the session's trace path.
	TraceOwnerID string
	// ProposalID names the sealed proposal this attempt produced, when it
	// produced one.
	ProposalID string
}

// payload is the recorded shape of one attempt. Kept separate so the ledger and
// a test can pin the same key set.
func (a DispatchAttempt) payload() map[string]any {
	return map[string]any{
		"attempt":        a.Attempt,
		"status":         a.Status,
		"retryable":      a.Retryable,
		"failure_class":  a.FailureClass,
		"scope_id":       a.ScopeID,
		"trace_owner_id": a.TraceOwnerID,
		"proposal_id":    a.ProposalID,
	}
}

// attemptEvidenceFor renders the attempt evidence for a finished dispatch. The
// attempt number and the classification both come from the envelope the caller
// already receives, so the fact and the result envelope can never disagree.
func attemptEvidenceFor(res DispatchResult) DispatchAttempt {
	env := res.Envelope()
	attempt := env.Attempts
	if attempt < 1 {
		attempt = 1
	}
	return DispatchAttempt{
		Attempt:      attempt,
		Status:       env.Status,
		Retryable:    RetryCandidate(env.FailureClass),
		FailureClass: env.FailureClass,
		ProposalID:   env.ProposalID,
	}
}

// recordDispatchAttemptFact appends one attempt's evidence to the trace. A
// failure to record is advisory: the dispatch already happened, so it is logged
// at debug and dropped, exactly as the world fact is.
//
// The append runs on an uncancelled context: the attempt whose evidence matters
// most is the one that was cancelled or timed out, and those are precisely the
// calls whose ctx is already done.
func recordDispatchAttemptFact(ctx context.Context, mgr *goatherd.ScopeManager, scopeID string, a DispatchAttempt) {
	if mgr == nil || scopeID == "" {
		return
	}
	scope, ok := mgr.Get(scopeID)
	if !ok {
		return
	}
	if a.ScopeID == "" {
		a.ScopeID = scope.ID()
	}
	if a.TraceOwnerID == "" {
		a.TraceOwnerID = scope.OwnerID()
	}
	if _, err := scope.AppendFact(context.WithoutCancel(ctx), SchemaDispatchAttempt, dispatchAttemptKind, a.payload()); err != nil {
		slog.Debug("dispatch attempt fact: append failed", "scope", scope.ID(), "err", err)
	}
}

// finishDispatch returns res after recording the attempt evidence for it. It is
// the single attempt boundary every foreground lane of Run passes through, so
// the evidence is emitted once per attempt rather than once per lane.
//
// A dispatch with no retained runtime has no trace wiring to append to (the
// shared lane of a session that never built one), and records nothing: the fact
// is evidence, not a requirement.
func finishDispatch(ctx context.Context, req DispatchRequest, res DispatchResult) DispatchResult {
	if req.Retained != nil {
		recordDispatchAttemptFact(ctx, req.Retained.ScopeManager, req.Retained.RootScopeID, attemptEvidenceFor(res))
	}
	return res
}
