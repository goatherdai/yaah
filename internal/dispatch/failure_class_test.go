package dispatch

import (
	"testing"

	"github.com/goatherdai/yaah/internal/agent/errorclassify"
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
