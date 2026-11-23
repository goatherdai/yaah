package dispatch

// retry_test.go pins the dispatch-boundary retry loop (yaah-dgkh): the lane
// matrix from the decision doc, the retry-candidate rule, the total-budget and
// backoff bounds, and the attempt aggregation on the final envelope.

import (
	"context"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/config"
)

// TestRetryLaneMatrix pins the decision's lane table.
func TestRetryLaneMatrix(t *testing.T) {
	p := newRetryPolicy(config.RetryConfig{MaxAttempts: 3})
	cases := []struct {
		name  string
		lane  Lane
		class string
		want  bool
	}{
		{"preflight retries", LaneProposal, FailureClassPreflightTransient, true},
		{"report retries a timeout", LaneReport, FailureClassTimeout, true},
		{"report retries a provider transient", LaneReport, FailureClassProviderTransient, true},
		{"report retries a stuck child", LaneReport, FailureClassStuck, true},
		{"report retries a preflight failure", LaneReport, FailureClassPreflightTransient, true},
		{"proposal never retries after a timeout", LaneProposal, FailureClassTimeout, false},
		{"proposal never retries a stuck child", LaneProposal, FailureClassStuck, false},
		{"proposal never retries a provider transient", LaneProposal, FailureClassProviderTransient, false},
		{"shared never retries", LaneShared, FailureClassPreflightTransient, false},
		{"shared never retries a timeout", LaneShared, FailureClassTimeout, false},
		{"cancelled is never retried", LaneReport, FailureClassCancelled, false},
		{"sealed is never retried", LaneReport, FailureClassSealed, false},
		{"content is never retried", LaneReport, FailureClassContent, false},
		{"configuration is never retried", LaneReport, FailureClassConfiguration, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.shouldRetry(tc.lane, tc.class, 1, attemptTerms{remaining: time.Minute, timed: true}); got != tc.want {
				t.Errorf("shouldRetry(%s, %s) = %v, want %v", tc.lane, tc.class, got, tc.want)
			}
		})
	}
}

// TestRetryBounds pins the attempt cap, the `only` filter, and the two time
// bounds (the dispatch budget and max_elapsed_seconds).
func TestRetryBounds(t *testing.T) {
	t.Run("attempt cap", func(t *testing.T) {
		p := newRetryPolicy(config.RetryConfig{MaxAttempts: 2})
		if !p.shouldRetry(LaneReport, FailureClassTimeout, 1, attemptTerms{remaining: time.Minute, timed: true}) {
			t.Fatal("first attempt must be retryable")
		}
		if p.shouldRetry(LaneReport, FailureClassTimeout, 2, attemptTerms{remaining: time.Minute, timed: true}) {
			t.Fatal("attempt 2 of 2 must not be retried")
		}
	})

	t.Run("only narrows the eligible classes", func(t *testing.T) {
		p := newRetryPolicy(config.RetryConfig{MaxAttempts: 3, Only: []string{FailureClassTimeout}})
		if !p.shouldRetry(LaneReport, FailureClassTimeout, 1, attemptTerms{remaining: time.Minute, timed: true}) {
			t.Fatal("timeout is in `only` and must be retryable")
		}
		if p.shouldRetry(LaneReport, FailureClassProviderTransient, 1, attemptTerms{remaining: time.Minute, timed: true}) {
			t.Fatal("provider_transient is not in `only` and must not be retryable")
		}
		// A typo can only narrow the policy, never widen it.
		typo := newRetryPolicy(config.RetryConfig{MaxAttempts: 3, Only: []string{"timout"}})
		if typo.shouldRetry(LaneReport, FailureClassTimeout, 1, attemptTerms{remaining: time.Minute, timed: true}) {
			t.Fatal("an unknown class name must make the policy narrower, not wider")
		}
	})

	t.Run("spent budget stops the retry", func(t *testing.T) {
		p := newRetryPolicy(config.RetryConfig{MaxAttempts: 3})
		if p.shouldRetry(LaneReport, FailureClassTimeout, 1, attemptTerms{remaining: 0, timed: true}) {
			t.Fatal("no time left must not be retried")
		}
		if !p.shouldRetry(LaneReport, FailureClassTimeout, 1, attemptTerms{remaining: 0, timed: false}) {
			t.Fatal("an untimed dispatch has no budget to exhaust")
		}
	})

	t.Run("max_elapsed bounds the backoff", func(t *testing.T) {
		p := newRetryPolicy(config.RetryConfig{MaxAttempts: 3, BackoffSecs: 10, MaxElapsedSeconds: 12})
		wait, ok := p.backoffFor(0, time.Minute, true)
		if !ok || wait != 10*time.Second {
			t.Fatalf("first wait = %v ok=%v, want 10s", wait, ok)
		}
		wait, ok = p.backoffFor(10*time.Second, time.Minute, true)
		if !ok || wait != 2*time.Second {
			t.Fatalf("second wait = %v ok=%v, want the 2s the bound left", wait, ok)
		}
		if _, ok = p.backoffFor(12*time.Second, time.Minute, true); ok {
			t.Fatal("an exhausted elapsed bound must stop the retry")
		}
		// A wait that would eat the whole dispatch remainder is not worth one.
		if _, ok = p.backoffFor(0, 5*time.Second, true); ok {
			t.Fatal("a backoff longer than the remaining dispatch budget must stop the retry")
		}
	})
}

// TestRetryPolicyForRole pins the per-role override: a role that declares a
// stanza replaces the global policy rather than merging into it.
func TestRetryPolicyForRole(t *testing.T) {
	rt := &RetainedRuntime{
		Retry: config.RetryConfig{MaxAttempts: 3},
		RetryForRole: func(role string) (config.RetryConfig, bool) {
			if role == "reviewer" {
				return config.RetryConfig{MaxAttempts: 1}, true
			}
			return config.RetryConfig{}, false
		},
	}
	if got := retryPolicyFor(rt, "developer").maxAttempts; got != 3 {
		t.Errorf("developer maxAttempts = %d, want the global 3", got)
	}
	if got := retryPolicyFor(rt, "reviewer").maxAttempts; got != 1 {
		t.Errorf("reviewer maxAttempts = %d, want its own 1", got)
	}
	if retryPolicyFor(nil, "developer").enabled() {
		t.Error("a dispatch with no retained runtime (the shared lane) must never retry")
	}
}

// fakeAttempt builds an attempt function that replays the given results.
func fakeAttempt(results ...DispatchResult) (func(context.Context, time.Duration, int) DispatchResult, *[]time.Duration) {
	var seen []time.Duration
	return func(_ context.Context, timeout time.Duration, n int) DispatchResult {
		seen = append(seen, timeout)
		if n-1 < len(results) {
			return results[n-1]
		}
		return results[len(results)-1]
	}, &seen
}

func retryTestRequest(timeout time.Duration) DispatchRequest {
	return DispatchRequest{
		Lane:        LaneReport,
		Params:      SubAgentParams{Role: "developer"},
		Prompt:      "do the work",
		Timeout:     timeout,
		Description: "widget",
	}
}

// TestRunAttemptsAggregates pins that a retried dispatch's final envelope names
// every attempt in order, counts them, and says it was retried.
func TestRunAttemptsAggregates(t *testing.T) {
	failed := DispatchResult{
		Status: DispatchStuck,
		Role:   "developer",
		Err:    ErrStuckChild,
		Meta:   EnvelopeMeta{DispatchID: "developer#1", Role: "developer", Lane: string(LaneReport)},
	}
	ok := DispatchResult{
		Status: DispatchCompleted,
		Role:   "developer",
		Result: "done",
		Meta:   EnvelopeMeta{DispatchID: "developer#1", Role: "developer", Lane: string(LaneReport)},
	}
	fn, seen := fakeAttempt(failed, ok)
	res := runAttempts(context.Background(), retryTestRequest(time.Minute), LaneReport,
		time.Now().Add(time.Minute), newRetryPolicy(config.RetryConfig{MaxAttempts: 2}), fn)

	if len(*seen) != 2 {
		t.Fatalf("attempts = %d, want 2", len(*seen))
	}
	if (*seen)[0] >= time.Minute || (*seen)[0] <= 0 {
		t.Errorf("first attempt timeout = %v, want a share of the budget: a timed-out attempt must leave room for its retry", (*seen)[0])
	}
	if (*seen)[1] <= 0 || (*seen)[1] > time.Minute {
		t.Errorf("retry timeout = %v, want a positive remainder of the budget (bounded by the dispatch budget)", (*seen)[1])
	}
	env := res.Envelope()
	if env.Attempts != 2 || !env.Retried {
		t.Fatalf("attempts = %d retried = %v, want 2/true", env.Attempts, env.Retried)
	}
	want := []string{"developer#1", "developer#1:attempt-2"}
	if len(env.AttemptIDs) != 2 || env.AttemptIDs[0] != want[0] || env.AttemptIDs[1] != want[1] {
		t.Errorf("attempt_ids = %v, want %v", env.AttemptIDs, want)
	}
	if res.Result != "done" {
		t.Errorf("result = %q, want the last attempt's", res.Result)
	}
}

// TestRunAttemptsStops pins the ends of the loop: a final class, a disabled
// policy, and an exhausted total budget each stop after the attempt that ran.
func TestRunAttemptsStops(t *testing.T) {
	cancelled := DispatchResult{
		Status: DispatchCancelled,
		Err:    context.Canceled,
		Meta:   EnvelopeMeta{DispatchID: "developer#2", Lane: string(LaneReport)},
	}
	t.Run("cancelled is final", func(t *testing.T) {
		fn, seen := fakeAttempt(cancelled)
		res := runAttempts(context.Background(), retryTestRequest(0), LaneReport, time.Time{},
			newRetryPolicy(config.RetryConfig{MaxAttempts: 3}), fn)
		if len(*seen) != 1 {
			t.Fatalf("attempts = %d, want 1 (a cancellation is deliberate)", len(*seen))
		}
		env := res.Envelope()
		if env.Attempts != 1 || env.Retried {
			t.Errorf("attempts = %d retried = %v, want a single attempt", env.Attempts, env.Retried)
		}
		if env.FailureClass != FailureClassCancelled {
			t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassCancelled)
		}
	})

	t.Run("no retry configured is one attempt", func(t *testing.T) {
		fn, seen := fakeAttempt(cancelled)
		res := runAttempts(context.Background(), retryTestRequest(0), LaneReport, time.Time{},
			noRetryPolicy, fn)
		if len(*seen) != 1 {
			t.Fatalf("attempts = %d, want 1 with no retry configured", len(*seen))
		}
		if ids := res.Envelope().AttemptIDs; len(ids) != 1 || ids[0] != "developer#2" {
			t.Errorf("attempt_ids = %v, want [developer#2]", ids)
		}
	})

	t.Run("total timeout expiry stops the sequence", func(t *testing.T) {
		failed := DispatchResult{
			Status: DispatchTimedOut,
			Err:    context.DeadlineExceeded,
			Meta:   EnvelopeMeta{DispatchID: "developer#3", Lane: string(LaneReport)},
		}
		fn, seen := fakeAttempt(failed)
		res := runAttempts(context.Background(), retryTestRequest(time.Millisecond), LaneReport,
			time.Now().Add(-time.Second), newRetryPolicy(config.RetryConfig{MaxAttempts: 3}), fn)
		if len(*seen) != 1 {
			t.Fatalf("attempts = %d, want 1 when the total budget is spent", len(*seen))
		}
		if got := res.Envelope().FailureClass; got != FailureClassTimeout {
			t.Errorf("failure_class = %q, want %q", got, FailureClassTimeout)
		}
	})
}

// TestRunRetriesReportLane proves the loop is wired into Run's report lane: the
// stuck attempt is re-run, the second succeeds, and both attempts record their
// own evidence.
func TestRunRetriesReportLane(t *testing.T) {
	fx, repo, rootID := newSessionFixtures(t)
	runner := &sessionRunner{responses: []runnerResponse{{err: ErrStuckChild}, {result: "second time"}}}
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
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != DispatchCompleted {
		t.Fatalf("status = %q, want %q", res.Status, DispatchCompleted)
	}
	if res.Result != "second time" {
		t.Errorf("result = %q, want the retry's report", res.Result)
	}
	env := res.Envelope()
	if env.Attempts != 2 || !env.Retried {
		t.Fatalf("attempts = %d retried = %v, want 2/true", env.Attempts, env.Retried)
	}

	root, ok := fx.mgr.Get(rootID)
	if !ok {
		t.Fatal("root scope missing")
	}
	facts := findRetainedFacts(t, fx.store, root.OwnerID(), SchemaDispatchAttempt)
	if len(facts) != 2 {
		t.Fatalf("attempt facts = %d, want one per attempt", len(facts))
	}
	counts := map[int]bool{}
	for _, rec := range facts {
		counts[toAttemptInt(rec.Body.Payload["attempt"])] = true
	}
	if !counts[1] || !counts[2] {
		t.Errorf("attempt numbers = %v, want 1 and 2", counts)
	}
}

// TestRunDoesNotRetrySharedLane pins that a shared dispatch is never re-run,
// even with the policy on: it mutates the host directly.
func TestRunDoesNotRetrySharedLane(t *testing.T) {
	calls := 0
	runner := func(context.Context, string, SubAgentParams) (string, error) {
		calls++
		return "", ErrStuckChild
	}
	res, err := Run(context.Background(), DispatchRequest{
		Lane:     LaneShared,
		Params:   SubAgentParams{Role: "developer"},
		Prompt:   "write it",
		Runner:   runner,
		Retained: &RetainedRuntime{Retry: config.RetryConfig{MaxAttempts: 3}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("runner calls = %d, want 1: the shared lane mutates the host", calls)
	}
	if res.Envelope().Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Envelope().Attempts)
	}
}
