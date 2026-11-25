package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent/errorclassify"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// TestFailureClass pins the dispatch-level mapping (yaah-i5k8): every class is
// reachable, "" means "did not fail", and the four retry candidates are exactly
// the first four.
func TestFailureClass(t *testing.T) {
	cases := []struct {
		name string
		in   FailureInput
		want string
	}{
		{"completed", FailureInput{Status: EnvelopeStatusProposal, StopReason: "completed"}, ""},
		{"no stop reason, no failure", FailureInput{Status: EnvelopeStatusRunning}, ""},
		{"settlement", FailureInput{Status: "settled"}, ""},

		{"preflight", FailureInput{Status: EnvelopeStatusError, Preflight: true}, FailureClassPreflightTransient},
		{"preflight wins over stop reason", FailureInput{Status: EnvelopeStatusError, StopReason: "stuck", Preflight: true}, FailureClassPreflightTransient},

		{"timeout", FailureInput{Status: EnvelopeStatusError, StopReason: "timeout"}, FailureClassTimeout},
		{"stuck", FailureInput{Status: EnvelopeStatusError, StopReason: "stuck"}, FailureClassStuck},
		{"canceled", FailureInput{Status: EnvelopeStatusError, StopReason: "canceled"}, FailureClassCancelled},
		{"cancelled spelling", FailureInput{Status: EnvelopeStatusError, StopReason: "cancelled"}, FailureClassCancelled},
		{"halted", FailureInput{Status: EnvelopeStatusError, StopReason: "halted"}, FailureClassCancelled},

		{"turn budget", FailureInput{Status: EnvelopeStatusProposal, StopReason: "turn_budget"}, FailureClassSealed},
		{"iteration budget", FailureInput{Status: EnvelopeStatusProposal, StopReason: "iteration_budget"}, FailureClassSealed},
		{"max tokens", FailureInput{Status: EnvelopeStatusReport, StopReason: "max_tokens"}, FailureClassSealed},

		{"provider error, unclassified", FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error"}, FailureClassProviderTransient},
		{"unclassified failure", FailureInput{Status: EnvelopeStatusError}, FailureClassProviderTransient},
		{"unknown stop reason", FailureInput{Status: EnvelopeStatusError, StopReason: "something_new"}, FailureClassConfiguration},

		{
			"provider rate limit",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonRateLimit, Retryable: true}},
			FailureClassProviderTransient,
		},
		{
			"provider timeout",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonTimeout, Retryable: true}},
			FailureClassTimeout,
		},
		{
			"content policy",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonContentPolicyBlocked}},
			FailureClassContent,
		},
		{
			"provider policy",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonProviderPolicyBlocked}},
			FailureClassContent,
		},
		{
			"auth",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonAuth}},
			FailureClassConfiguration,
		},
		{
			"model not found",
			FailureInput{Status: EnvelopeStatusError, StopReason: "provider_error", Classified: &errorclassify.ClassifiedError{Reason: errorclassify.ReasonModelNotFound}},
			FailureClassConfiguration,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureClass(tc.in); got != tc.want {
				t.Errorf("FailureClass(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRetryCandidate pins the boundary the retry policy reads (yaah-dgkh):
// exactly the first four classes are re-runnable, and "no failure" is not.
func TestRetryCandidate(t *testing.T) {
	retryable := map[string]bool{
		FailureClassPreflightTransient: true,
		FailureClassProviderTransient:  true,
		FailureClassTimeout:            true,
		FailureClassStuck:              true,
		FailureClassContent:            false,
		FailureClassConfiguration:      false,
		FailureClassCancelled:          false,
		FailureClassSealed:             false,
		"":                             false,
	}
	for class, want := range retryable {
		if got := RetryCandidate(class); got != want {
			t.Errorf("RetryCandidate(%q) = %v, want %v", class, got, want)
		}
	}
}

// TestSupervisionRefusalIsConfiguration pins the classification of a supervised
// dispatch's start refusals (yaah-ugtl review M3): a failed pre-run capture and
// a workspace that can never roll back are deterministic config or lane
// outcomes. Left to the provider layer they read as an unknown error --
// retryable by default -- so a report-lane retry policy would re-provision a
// world per attempt for a dispatch that can never start.
func TestSupervisionRefusalIsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sentinel error
	}{
		{"capture failed", supervisionapi.ErrSupervisedCaptureFailed},
		{"not reversible", supervisionapi.ErrSupervisedNotReversible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The shape the runner returns: the role, the sentinel via %w, the
			// underlying cause via %v.
			err := fmt.Errorf("sub-agent developer: %w: %v", tc.sentinel, errors.New("worktree snapshot refused: no space left on device"))
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("the wrapped refusal lost its sentinel: %v", err)
			}
			if got := causeFailureClass(err); got != FailureClassConfiguration {
				t.Errorf("causeFailureClass(%v) = %q, want %q", err, got, FailureClassConfiguration)
			}
			// The attempt boundary reads the same answer with the stage a
			// report-lane body failure carries: the child never ran.
			if got := failedAttemptClass(context.Background(), RetainedStageBody, DispatchResult{Status: DispatchFailed}, err); got != FailureClassConfiguration {
				t.Errorf("failedAttemptClass = %q, want %q", got, FailureClassConfiguration)
			}
		})
	}
}

// TestSupervisionRefusalIsNotRetried pins the consequence at the retry boundary:
// a report-lane dispatch whose body fails with a supervision refusal runs
// exactly once under a three-attempt policy, and its envelope class says
// configuration.
func TestSupervisionRefusalIsNotRetried(t *testing.T) {
	err := fmt.Errorf("sub-agent developer: %w: %v", supervisionapi.ErrSupervisedCaptureFailed, errors.New("no space left on device"))
	refused := DispatchResult{
		Status:       DispatchFailed,
		Role:         "developer",
		Err:          err,
		FailureClass: causeFailureClass(err),
		Meta:         EnvelopeMeta{DispatchID: "developer#1", Role: "developer", Lane: string(LaneReport)},
	}
	fn, seen := fakeAttempt(refused)
	res := runAttempts(context.Background(), retryTestRequest(0), LaneReport, time.Time{},
		newRetryPolicy(config.RetryConfig{MaxAttempts: 3}), fn)
	if len(*seen) != 1 {
		t.Fatalf("attempts = %d, want 1: a deterministic start refusal must not be re-provisioned", len(*seen))
	}
	env := res.Envelope()
	if env.Attempts != 1 || env.Retried {
		t.Errorf("attempts = %d retried = %v, want a single attempt", env.Attempts, env.Retried)
	}
	if env.FailureClass != FailureClassConfiguration {
		t.Errorf("failure_class = %q, want %q", env.FailureClass, FailureClassConfiguration)
	}
}
