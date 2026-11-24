package dispatch

// review_regression_test.go: focused regressions for the yaah-dgkh/yaah-i5k8
// code-review findings that the first remediation pass fixed in code but did not
// yet pin with tests.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/errorclassify"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/jobs"
)

// TestFailedAttemptClass pins the precedence of the dispatch boundary's final
// classification: a cancelled parent outranks everything, a typed retained stage
// names preflight_transient/configuration, and an unclassified failed dispatch
// falls through to the cause's provider classification.
func TestFailedAttemptClass(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// #2: a parent that has gone away is cancelled, whatever the failing call said.
	if got := failedAttemptClass(cancelled, RetainedStageProvisioning, DispatchResult{}, nil); got != FailureClassCancelled {
		t.Errorf("cancelled ctx = %q, want %q", got, FailureClassCancelled)
	}

	// #5: retryable provisioning vs permanent refusal vs a body that already ran.
	if got := failedAttemptClass(context.Background(), RetainedStageProvisioning, DispatchResult{}, nil); got != FailureClassPreflightTransient {
		t.Errorf("provisioning = %q, want %q", got, FailureClassPreflightTransient)
	}
	if got := failedAttemptClass(context.Background(), RetainedStageRefused, DispatchResult{}, nil); got != FailureClassConfiguration {
		t.Errorf("refused = %q, want %q", got, FailureClassConfiguration)
	}
	if got := failedAttemptClass(context.Background(), RetainedStageBody, DispatchResult{}, nil); got != "" {
		t.Errorf("body = %q, want empty (never a preflight failure)", got)
	}
	if got := failedAttemptClass(context.Background(), RetainedStageSeal, DispatchResult{}, nil); got != "" {
		t.Errorf("seal = %q, want empty (no artifact to classify yet)", got)
	}

	// #4: an unclassified failed dispatch classifies by its cause, not as transient.
	auth := fmt.Errorf("wrap: %w", &errorclassify.ClassifiedError{Reason: errorclassify.ReasonAuth})
	if got := failedAttemptClass(context.Background(), RetainedStageNone, DispatchResult{Status: DispatchFailed}, auth); got != FailureClassConfiguration {
		t.Errorf("auth cause = %q, want %q", got, FailureClassConfiguration)
	}
}

// TestCauseFailureClass pins that a terminal provider failure is lowered to a
// non-retryable class instead of reading as provider_transient.
func TestCauseFailureClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"auth", fmt.Errorf("wrap: %w", &errorclassify.ClassifiedError{Reason: errorclassify.ReasonAuth}), FailureClassConfiguration},
		{"content policy", fmt.Errorf("wrap: %w", &errorclassify.ClassifiedError{Reason: errorclassify.ReasonContentPolicyBlocked}), FailureClassContent},
		{"rate limit", fmt.Errorf("wrap: %w", &errorclassify.ClassifiedError{Reason: errorclassify.ReasonRateLimit, Retryable: true}), FailureClassProviderTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := causeFailureClass(tc.err); got != tc.want {
				t.Errorf("causeFailureClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestAttemptAllowance pins the per-attempt budget split that makes timeout a
// retryable class: with retry enabled, attempt 1 gets a share, not the whole
// budget.
func TestAttemptAllowance(t *testing.T) {
	// Untimed: no budget to split.
	if got := attemptAllowance(time.Minute, time.Minute, false, 1, 1, true); got != time.Minute {
		t.Errorf("untimed = %v, want the whole budget", got)
	}
	// Split lane (report: timeout is retryable).
	if got := attemptAllowance(time.Minute, time.Minute, true, 1, 3, true); got != time.Minute/3 {
		t.Errorf("attempt 1 of 3 = %v, want %v", got, time.Minute/3)
	}
	if got := attemptAllowance(time.Minute, 40*time.Second, true, 2, 3, true); got != 20*time.Second {
		t.Errorf("attempt 2 of 3 = %v, want 20s (half the remainder)", got)
	}
	if got := attemptAllowance(time.Minute, 20*time.Second, true, 3, 3, true); got != 20*time.Second {
		t.Errorf("attempt 3 of 3 = %v, want the remainder", got)
	}
	// A zero or expired split share is still bounded, never "no limit".
	if got := attemptAllowance(time.Minute, 0, true, 1, 3, true); got <= 0 {
		t.Errorf("expired split share = %v, want a positive nanosecond bound", got)
	}
	// Non-split lane (proposal/shared: timeout is terminal).
	if got := attemptAllowance(time.Minute, time.Minute, true, 1, 3, false); got != time.Minute {
		t.Errorf("non-split attempt 1 = %v, want the full budget", got)
	}
	if got := attemptAllowance(time.Minute, 40*time.Second, true, 2, 3, false); got != 40*time.Second {
		t.Errorf("non-split retry = %v, want the remaining 40s", got)
	}
	if got := attemptAllowance(time.Minute, 0, true, 2, 3, false); got <= 0 {
		t.Errorf("non-split spent retry = %v, want a positive nanosecond bound", got)
	}
}

// TestMayRetryClass pins the split gate: the budget is divided only when a
// timed-out attempt could actually be re-run. An `only` list that excludes
// timeout must not shorten the first attempt for a retry that cannot happen.
func TestMayRetryClass(t *testing.T) {
	all := newRetryPolicy(config.RetryConfig{MaxAttempts: 3})
	if !all.mayRetryClass(LaneReport, FailureClassTimeout) {
		t.Error("timeout on report lane = false, want true (no `only` filter admits every candidate)")
	}
	if all.mayRetryClass(LaneReport, FailureClassContent) {
		t.Error("content on report lane = true, want false (content is never retryable)")
	}

	noTimeout := newRetryPolicy(config.RetryConfig{MaxAttempts: 3, Only: []string{FailureClassProviderTransient}})
	if noTimeout.mayRetryClass(LaneReport, FailureClassTimeout) {
		t.Error("timeout on report lane = true, want false (`only` excludes timeout)")
	}
	if !noTimeout.mayRetryClass(LaneReport, FailureClassProviderTransient) {
		t.Error("provider_transient on report lane = false, want true (`only` admits it)")
	}

	single := newRetryPolicy(config.RetryConfig{MaxAttempts: 1})
	if single.mayRetryClass(LaneReport, FailureClassTimeout) {
		t.Error("timeout on report lane = true, want false (single attempt cannot retry)")
	}
}

// TestRunAttemptsPassesRemainingBudget pins the fallback-budget plumbing: the
// loop hands each attempt its divided share *and* the overall budget remaining
// on the dispatch, so a report that falls back to the non-retryable session
// container can run against the whole budget instead of its divided slice.
func TestRunAttemptsPassesRemainingBudget(t *testing.T) {
	failed := DispatchResult{
		Status: DispatchStuck,
		Err:    ErrStuckChild,
		Meta:   EnvelopeMeta{DispatchID: "d#1", Lane: string(LaneReport)},
	}
	ok := DispatchResult{
		Status: DispatchCompleted,
		Result: "done",
		Meta:   EnvelopeMeta{DispatchID: "d#1", Lane: string(LaneReport)},
	}
	var got []struct{ timeout, remaining time.Duration }
	attempt := func(_ context.Context, timeout, remaining time.Duration, n int) DispatchResult {
		got = append(got, struct{ timeout, remaining time.Duration }{timeout, remaining})
		if n == 1 {
			return failed
		}
		return ok
	}
	runAttempts(context.Background(), retryTestRequest(time.Minute), LaneReport,
		time.Now().Add(time.Minute), newRetryPolicy(config.RetryConfig{MaxAttempts: 2}), attempt)

	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if got[0].timeout >= time.Minute || got[0].timeout <= 0 {
		t.Errorf("first attempt share = %v, want a strict slice of the budget", got[0].timeout)
	}
	if got[0].remaining <= 0 || got[0].remaining > time.Minute {
		t.Errorf("remaining budget handed to the attempt = %v, want the full (bounded) dispatch budget", got[0].remaining)
	}
	if got[0].remaining <= got[0].timeout {
		t.Errorf("remaining = %v must exceed the divided share = %v so a fallback can widen to it", got[0].remaining, got[0].timeout)
	}
}

// TestSetRetryPolicy pins the workflow-side wiring: the global stanza is
// installed, and a role override replaces it rather than merging.
func TestSetRetryPolicy(t *testing.T) {
	rt := &RetainedRuntime{}
	rt.SetRetryPolicy(config.SubAgentConfig{
		Retry: config.RetryConfig{MaxAttempts: 3},
		Roles: map[string]config.RoleConfig{
			"reviewer": {Retry: &config.RetryConfig{MaxAttempts: 1}},
		},
	})
	if rt.Retry.MaxAttempts != 3 {
		t.Errorf("global maxAttempts = %d, want 3", rt.Retry.MaxAttempts)
	}
	if rt.RetryForRole == nil {
		t.Fatal("RetryForRole not wired")
	}
	if rc, ok := rt.RetryForRole("reviewer"); !ok || rc.MaxAttempts != 1 {
		t.Errorf("reviewer override = %v ok=%v, want maxAttempts 1", rc, ok)
	}
	if _, ok := rt.RetryForRole("developer"); ok {
		t.Error("developer must inherit the global policy, not override it")
	}
}

// TestAttemptEvidenceIdentity pins that a fact carries the ids that join it to
// its envelope on a shared root scope.
func TestAttemptEvidenceIdentity(t *testing.T) {
	res := DispatchResult{
		Status:     DispatchCompleted,
		Attempt:    2,
		AttemptIDs: []string{"developer#9", "developer#9:attempt-2"},
		Meta:       EnvelopeMeta{DispatchID: "developer#9", Role: "developer", Lane: string(LaneReport)},
	}
	a := attemptEvidenceFor(res)
	if a.DispatchID != "developer#9" {
		t.Errorf("dispatch_id = %q, want developer#9", a.DispatchID)
	}
	if a.AttemptID != "developer#9:attempt-2" {
		t.Errorf("attempt_id = %q, want developer#9:attempt-2", a.AttemptID)
	}
	if a.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", a.Attempt)
	}
}

// TestCarryWarnings pins that a carried warning lands on both the typed list and
// the single-line summary.
func TestCarryWarnings(t *testing.T) {
	env := carryWarnings(proposalEnvelope{}, []string{"a", "b"})
	if len(env.Warnings) != 2 || env.Warnings[0] != "a" || env.Warnings[1] != "b" {
		t.Errorf("warnings = %v, want [a b]", env.Warnings)
	}
	if env.Warning != "a" {
		t.Errorf("warning = %q, want a (first)", env.Warning)
	}
}

// TestRunAttemptsCarriesWarnings pins the end-to-end behavior: a stranded
// worktree warning from a failed attempt survives a successful retry.
func TestRunAttemptsCarriesWarnings(t *testing.T) {
	failed := DispatchResult{
		Status:   DispatchStuck,
		Err:      ErrStuckChild,
		Warnings: []string{"stranded worktree"},
		Meta:     EnvelopeMeta{DispatchID: "d#w", Lane: string(LaneReport)},
	}
	ok := DispatchResult{
		Status: DispatchCompleted,
		Result: "done",
		Meta:   EnvelopeMeta{DispatchID: "d#w", Lane: string(LaneReport)},
	}
	fn, _ := fakeAttempt(failed, ok)
	res := runAttempts(context.Background(), retryTestRequest(0), LaneReport, time.Time{},
		newRetryPolicy(config.RetryConfig{MaxAttempts: 2}), fn)
	env := res.Envelope()
	if len(env.Warnings) != 1 || env.Warnings[0] != "stranded worktree" {
		t.Errorf("warnings = %v, want [stranded worktree] carried across the retry", env.Warnings)
	}
}

// TestRunAttemptsCancellationAggregates pins that a cancellation during backoff
// still reports the whole sequence's aggregated duration, not just the last
// attempt's.
func TestRunAttemptsCancellationAggregates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n int
	attempt := func(context.Context, time.Duration, time.Duration, int) DispatchResult {
		n++
		if n == 2 {
			time.AfterFunc(20*time.Millisecond, cancel)
		}
		return DispatchResult{
			Status: DispatchTimedOut,
			Err:    context.DeadlineExceeded,
			Meta:   EnvelopeMeta{DispatchID: "d#c", Lane: string(LaneReport)},
		}
	}
	res := runAttempts(ctx, retryTestRequest(0), LaneReport, time.Time{},
		newRetryPolicy(config.RetryConfig{MaxAttempts: 3, BackoffSecs: 1}), attempt)
	env := res.Envelope()
	if env.Attempts != 2 || !env.Retried {
		t.Fatalf("attempts = %d retried = %v, want 2/true", env.Attempts, env.Retried)
	}
	if env.DurationMs <= 0 {
		t.Errorf("duration_ms = %d, want > 0 (aggregated after cancel-during-backoff)", env.DurationMs)
	}
}

// TestRecordBackgroundAttempt pins that a background job records its one attempt
// fact when it reaches a terminal state, on the session root scope.
func TestRecordBackgroundAttempt(t *testing.T) {
	fx, _, rootID := newSessionFixtures(t)
	rt := &RetainedRuntime{ScopeManager: fx.mgr, RootScopeID: rootID}
	rt.RecordBackgroundAttempt(context.Background(),
		jobs.BackgroundJobStatus{ID: "bg-9", Role: "developer", Status: "completed"},
		"result text", nil)

	root, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	facts := findRetainedFacts(t, fx.store, root.OwnerID(), SchemaDispatchAttempt)
	if len(facts) != 1 {
		t.Fatalf("recorded %d attempt facts, want exactly 1", len(facts))
	}
	p := facts[0].Body.Payload
	if p["dispatch_id"] != "bg-9" {
		t.Errorf("dispatch_id = %v, want bg-9", p["dispatch_id"])
	}
	if p["status"] != "completed" {
		t.Errorf("status = %v, want completed", p["status"])
	}
}

// TestRunTimeoutRetries pins that a timed-out attempt is classified timeout —
// not cancelled — and is retried on the report lane (yaah-dgkh review #4).
func TestRunTimeoutRetries(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	runner := &sessionRunner{responses: []runnerResponse{
		{err: context.DeadlineExceeded},
		{result: "second time"},
	}}
	rt := RetainedRuntime{
		Runner:       runner.run(),
		RepoPath:     repo,
		ScopeManager: fx.mgr,
		RootScopeID:  rootID,
		WorktreeRoot: t.TempDir(),
		Proposals:    NewProposalRegistry(),
		Retry:        config.RetryConfig{MaxAttempts: 2},
	}
	res, err := Run(context.Background(), DispatchRequest{
		Lane:     LaneReport,
		Params:   SubAgentParams{Role: "reviewer"},
		Prompt:   "review the thing",
		Retained: &rt,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := res.Envelope()
	if env.Attempts != 2 || !env.Retried {
		t.Fatalf("attempts = %d retried = %v, want 2/true: a timeout must be retried, not treated as cancelled", env.Attempts, env.Retried)
	}
	root, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	var attempt1Failure string
	found := false
	for _, rec := range findRetainedFacts(t, fx.store, root.OwnerID(), SchemaDispatchAttempt) {
		if toAttemptInt(rec.Body.Payload["attempt"]) == 1 {
			attempt1Failure, _ = rec.Body.Payload["failure_class"].(string)
			found = true
		}
	}
	if !found {
		t.Fatal("no attempt-1 fact recorded")
	}
	if attempt1Failure != FailureClassTimeout {
		t.Errorf("attempt 1 failure_class = %q, want %q (not cancelled)", attempt1Failure, FailureClassTimeout)
	}
}
