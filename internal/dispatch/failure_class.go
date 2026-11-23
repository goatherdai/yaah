package dispatch

import (
	"context"
	"errors"

	"github.com/goatherdai/yaah/internal/agent/errorclassify"
)

// failure_class.go is the dispatch-level failure taxonomy (yaah-i5k8, decision
// yaah-hxk3.5). The provider layer already classifies its own errors
// (internal/agent/errorclassify); this is the coarser question the dispatch
// boundary asks: if this attempt failed, may the retry policy run it again?
//
// The set is closed and ordered. The FIRST FOUR are the retry candidates — a
// dispatch-boundary retry may re-run an attempt that ended in one of them. The
// LAST FOUR are final for the retry policy: the stop was deliberate (cancelled,
// sealed) or re-running the attempt unchanged cannot help (content,
// configuration).

const (
	// FailureClassPreflightTransient: the attempt failed before its world ran
	// (provisioning, bootstrap, image, daemon). Nothing ran, so nothing was
	// left behind and a fresh attempt starts from the same place.
	FailureClassPreflightTransient = "preflight_transient"
	// FailureClassProviderTransient: the provider failed transiently
	// (overload, rate limit, 5xx, an unnamed cause). The provider layer has
	// already exhausted its own retries by the time this is reported.
	FailureClassProviderTransient = "provider_transient"
	// FailureClassTimeout: the attempt hit its wall-clock budget.
	FailureClassTimeout = "timeout"
	// FailureClassStuck: the child went silent and the stuck-child watchdog
	// killed it.
	FailureClassStuck = "stuck"

	// FailureClassContent: the provider refused the content (a policy block).
	// The prompt has to change; the same dispatch is refused again.
	FailureClassContent = "content"
	// FailureClassConfiguration: the dispatch was misconfigured — credentials,
	// billing, model name, request shape or budget. A config change fixes it;
	// a re-run of the same dispatch does not.
	FailureClassConfiguration = "configuration"
	// FailureClassCancelled: the parent deadline expired or the supervisor
	// halted the run. A deliberate stop; the lane rules never retry it.
	FailureClassCancelled = "cancelled"
	// FailureClassSealed: the run stopped at a budget with reviewable work
	// already sealed. A sealed partial is consume-once and reviewable, so the
	// recovery is continue/fork the proposal, not a fresh dispatch.
	FailureClassSealed = "sealed"
	// FailureClassSealFailed: the body ran but sealing failed. The runtime
	// kept a durable artifact with the worktree and conversation; recovery is
	// the proposal's recover/discard verbs, never a fresh dispatch.
	FailureClassSealFailed = "seal_failed"
)

// FailureInput is everything the dispatch core knows about how one attempt
// ended: the envelope's status and stop_reason, whether the attempt failed
// before its world ran, and — when the provider layer classified the run's
// error — that classification. Every field is optional; an input with nothing
// set describes a dispatch that did not fail and classifies as "".
type FailureInput struct {
	// Status is the envelope status the attempt reported. It matters only for
	// the empty stop_reason case: a failure (`error`) with no named stop reason
	// is unclassified, while a running accept or a settlement simply did not
	// fail.
	Status string
	// StopReason is the envelope's stop_reason vocabulary: completed,
	// turn_budget, iteration_budget, max_tokens, timeout, stuck, halted,
	// canceled, provider_error — or empty when the cause is arbitrary.
	StopReason string
	// Preflight reports that the attempt failed before its world ran. A lane
	// that knows this sets it, because nothing in the envelope says it: the
	// stop reason for a provisioning failure is as empty as any other.
	Preflight bool
	// Classified is the provider layer's classification of the run's error,
	// when it made one. It refines provider_error (and the unclassified case)
	// from "a provider failure" into transient/content/configuration.
	Classified *errorclassify.ClassifiedError
}

// FailureClass maps one attempt's outcome onto the dispatch failure taxonomy.
// It returns "" when the attempt did not fail — a completed dispatch, a running
// accept, a settlement — so an envelope without a failure class reads as "no
// failure", never as "failure class unknown".
//
// The mapping, in order of precedence:
//
//	Preflight                     -> preflight_transient
//	timeout                       -> timeout
//	stuck                         -> stuck
//	canceled / cancelled / halted -> cancelled
//	turn_budget / iteration_budget / max_tokens
//	                              -> sealed
//	completed                     -> "" (not a failure)
//	provider_error / "" (error)   -> from Classified, else provider_transient
//	anything else                 -> from Classified, else configuration
//
// A halt is classified `cancelled` because both stops are deliberate — the
// decision's lane rules say "parent cancellation / user halt: never retry" —
// and the four retryable classes must mean exactly what the retry policy
// reads them to mean.
func FailureClass(in FailureInput) string {
	if in.Preflight {
		return FailureClassPreflightTransient
	}
	switch in.StopReason {
	case "completed":
		return ""
	case "timeout":
		return FailureClassTimeout
	case "stuck":
		return FailureClassStuck
	case "canceled", "cancelled", "halted":
		return FailureClassCancelled
	case "turn_budget", "iteration_budget", "max_tokens":
		return FailureClassSealed
	case "", "provider_error":
		// Both fall through to the provider classification: provider_error has
		// one, and an unnamed failure may have one too.
		if in.StopReason == "" && in.Status != EnvelopeStatusError {
			// An empty stop reason on anything but an error envelope is not a
			// failure: a running accept, a settlement, a plain completion.
			return ""
		}
	}
	if class := providerFailureClass(in.Classified); class != "" {
		return class
	}
	if in.StopReason == "" || in.StopReason == "provider_error" {
		// An unclassified failure stays a retry candidate, matching
		// failureRecovery's existing treatment: the cause is unnamed, not
		// known final. A provider_error with no classification names the
		// provider as the cause.
		return FailureClassProviderTransient
	}
	// A stop reason this taxonomy does not know is not assumed transient.
	return FailureClassConfiguration
}

// providerFailureClass lowers the provider layer's classification onto the
// dispatch taxonomy, or "" when there is no classification to lower.
func providerFailureClass(c *errorclassify.ClassifiedError) string {
	if c == nil {
		return ""
	}
	switch c.Reason {
	case errorclassify.ReasonTimeout:
		// A transport/request timeout is the dispatch's timeout class even
		// though the provider layer calls it retryable.
		return FailureClassTimeout
	case errorclassify.ReasonContentPolicyBlocked, errorclassify.ReasonProviderPolicyBlocked:
		return FailureClassContent
	}
	if c.Retryable {
		return FailureClassProviderTransient
	}
	// Every other final provider failure — auth, billing, model not found, a
	// request shape or budget the dispatch's own config decides — is a
	// configuration problem rather than a refusal of the content.
	return FailureClassConfiguration
}

// RetryCandidate reports whether a failure class is one the dispatch-boundary
// retry policy may re-run: exactly the first four classes.
//
// It is deliberately not the same question as ResultRecovery.Retryable, which
// answers "is re-running worth anything?" for a human. A sealed partial is
// worth continuing (Retryable) but must never be re-dispatched automatically
// (not a retry candidate), and a cancelled run is worth repeating on request
// but was stopped deliberately.
func RetryCandidate(class string) bool {
	switch class {
	case FailureClassPreflightTransient, FailureClassProviderTransient,
		FailureClassTimeout, FailureClassStuck:
		return true
	}
	return false
}

// RetainedFailureStage names how far a retained start got before it failed
// (yaah-dgkh review). It is the typed answer to "did the child run?", which is
// what decides whether a retry can start from the same place: a failure before
// the world existed leaves nothing behind, and a failure after the body ran
// must never be called a preflight one.
type RetainedFailureStage string

const (
	// RetainedStageNone: the start did not fail, or a failure a later stage
	// already describes.
	RetainedStageNone RetainedFailureStage = ""
	// RetainedStageProvisioning: the dispatch world could not be set up — the
	// worktree, the container, its scope, its copy-in. Nothing ran, so a fresh
	// attempt starts from the same place; this is the one retryable stage.
	RetainedStageProvisioning RetainedFailureStage = "provisioning"
	// RetainedStageRefused: the dispatch was refused before any world existed —
	// a precondition, an unknown world, an authority declaration no plan
	// covers, a seed that cannot be threaded. A config change fixes it; a
	// re-run of the same dispatch does not.
	RetainedStageRefused RetainedFailureStage = "refused"
	// RetainedStageBody: the child ran and its body failed.
	RetainedStageBody RetainedFailureStage = "body"
	// RetainedStageSeal: the body ran and the sealing work after it failed
	// (the authority citation could not be resolved, the delta could not be
	// captured). The world existed and did work, so it is never a preflight
	// failure however transient the cause looks.
	RetainedStageSeal RetainedFailureStage = "seal"
)

// failureClass lowers a stage onto the dispatch taxonomy. "" means the stage
// says nothing the envelope's own status and stop_reason do not say better.
func (s RetainedFailureStage) failureClass() string {
	switch s {
	case RetainedStageProvisioning:
		return FailureClassPreflightTransient
	case RetainedStageRefused:
		return FailureClassConfiguration
	case RetainedStageSeal:
		return FailureClassSealFailed
	}
	return ""
}

// failedAttemptClass is the dispatch boundary's final word on how one attempt
// failed: the class that overrides the one the envelope derives. The precedence
// is deliberate.
//
//	ctx already cancelled -> cancelled: the parent went away, and a start can
//	                        fail *because* it did, so a deliberate stop
//	                        outranks whatever the failing call reported.
//	typed stage          -> preflight_transient (provisioning only) or
//	                        configuration (a permanent refusal).
//	unclassified failure -> the provider layer's classification of the cause,
//	                        so a terminal auth/billing/model-not-found/
//	                        content-policy failure is not retried as if it
//	                        were transient.
//
// "" means the envelope's status and stop_reason name the cause more precisely
// (a timeout, a stuck child, a deliberate cancellation), and its own
// derivation stands.
func failedAttemptClass(ctx context.Context, stage RetainedFailureStage, res DispatchResult, err error) string {
	if ctx.Err() != nil {
		return FailureClassCancelled
	}
	if class := stage.failureClass(); class != "" {
		return class
	}
	if res.Status != DispatchFailed || err == nil {
		return ""
	}
	return causeFailureClass(err)
}

// causeFailureClass classifies an arbitrary failure cause through the provider
// layer's taxonomy. Without it every unclassified failure would read as
// provider_transient and be retried, including the ones no retry can fix.
func causeFailureClass(err error) string {
	var classified *errorclassify.ClassifiedError
	c := errorclassify.Classify(err, errorclassify.ErrorMeta{})
	if errors.As(err, &classified) {
		c = *classified
	}
	return FailureClass(FailureInput{Status: EnvelopeStatusError, Classified: &c})
}
