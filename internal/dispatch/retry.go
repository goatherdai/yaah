package dispatch

// retry.go is the dispatch-boundary retry loop's policy and bookkeeping
// (yaah-dgkh, decision yaah-hxk3.5). The failure taxonomy it reads lives in
// failure_class.go; this file answers the next question — given a finished
// attempt, may the boundary run it again, and how.
//
// The decision is deliberately narrower than the provider layer's retry:
//
//   - a lane may refuse to be retried at all (the shared lane mutates the host),
//   - a failure class must be a retry candidate (RetryCandidate) and admitted by
//     the configured `only` list,
//   - the attempt count must be under the configured maximum, and
//   - the dispatch's total wall-clock budget must still have time in it.
//
// Every attempt keeps its own evidence (attempt_evidence.go records one fact per
// attempt through finishDispatch), and the final envelope aggregates the attempt
// count, the ids, and the wall clock across the whole sequence.

import (
	"context"
	"fmt"
	"time"

	"github.com/goatherdai/yaah/internal/config"
)

// retryPolicy is the resolved form of config.RetryConfig: what the loop asks
// rather than what the file says.
type retryPolicy struct {
	maxAttempts int
	backoff     time.Duration
	maxElapsed  time.Duration
	// only restricts the eligible classes to a configured subset. nil means
	// "every retry candidate", so the default needs no list of its own.
	only map[string]bool
}

// noRetryPolicy is the policy of a dispatch with no retry configured: exactly
// one attempt, the historical behavior.
var noRetryPolicy = retryPolicy{maxAttempts: 1}

// failureClasses is the closed taxonomy `only` is filtered against. The names
// are failure_class.go's; this list exists so a typo in config cannot widen the
// policy, only narrow it.
var failureClasses = []string{
	FailureClassPreflightTransient,
	FailureClassProviderTransient,
	FailureClassTimeout,
	FailureClassStuck,
	FailureClassContent,
	FailureClassConfiguration,
	FailureClassCancelled,
	FailureClassSealed,
}

// newRetryPolicy resolves a configured stanza into the policy the loop reads.
func newRetryPolicy(cfg config.RetryConfig) retryPolicy {
	p := retryPolicy{
		maxAttempts: cfg.Attempts(),
		backoff:     cfg.Backoff(),
		maxElapsed:  cfg.MaxElapsed(),
	}
	if len(cfg.Only) > 0 {
		p.only = make(map[string]bool, len(cfg.Only))
		for _, class := range cfg.Only {
			if knownFailureClass(class) {
				p.only[class] = true
			}
		}
	}
	return p
}

// knownFailureClass reports whether class names one of the taxonomy's classes.
func knownFailureClass(class string) bool {
	for _, c := range failureClasses {
		if c == class {
			return true
		}
	}
	return false
}

// enabled reports whether the policy may re-run an attempt at all.
func (p retryPolicy) enabled() bool { return p.maxAttempts > 1 }

// retryPolicyFor resolves one dispatch's policy: the role's override when the
// config supplies one, else the global stanza. A dispatch with no retained
// runtime is the shared lane — it has no world to re-provision and no config to
// read, so it never retries.
func retryPolicyFor(rt *RetainedRuntime, role string) retryPolicy {
	if rt == nil {
		return noRetryPolicy
	}
	cfg := rt.Retry
	if rt.RetryForRole != nil {
		if rc, ok := rt.RetryForRole(role); ok {
			cfg = rc
		}
	}
	return newRetryPolicy(cfg)
}

// laneMayRetry is the decision's lane matrix: whether a lane admits a re-run at
// all, given the class the attempt failed with.
//
//	report lane          every retry-candidate class: the attempt's world is
//	                     destroyed with the attempt, so nothing is left behind.
//	proposal lane        only preflight_transient — a failure before the world
//	                     ran. After a seal the proposal is reviewable and
//	                     consume-once, so a re-dispatch would duplicate work the
//	                     orchestrator can already settle.
//	shared lane (and any
//	unknown lane)       never: it mutates the host directly, so a partial write
//	                     is not something a retry can undo.
func laneMayRetry(lane Lane, class string) bool {
	switch lane {
	case LaneReport:
		return true
	case LaneProposal:
		return class == FailureClassPreflightTransient
	default:
		return false
	}
}

// shouldRetry is the whole retry decision for one finished attempt. attempts is
// how many attempts have run so far (1 for the first), remaining is the time
// left in the dispatch's total budget, and timed reports whether that budget
// exists at all (a dispatch with no timeout always "has time").
func (p retryPolicy) shouldRetry(lane Lane, class string, attempts int, remaining time.Duration, timed bool) bool {
	if !p.enabled() || attempts >= p.maxAttempts {
		return false
	}
	// The four final classes — content, configuration, cancelled, sealed — are
	// never re-run, whatever `only` says: the prompt or the config has to
	// change, or the stop was deliberate, or the work is already reviewable.
	if !RetryCandidate(class) {
		return false
	}
	if p.only != nil && !p.only[class] {
		return false
	}
	if !laneMayRetry(lane, class) {
		return false
	}
	if timed && remaining <= 0 {
		return false
	}
	return true
}

// backoffFor is how long to wait before the next attempt, and whether waiting
// is allowed at all. The wait is bounded twice: by max_elapsed_seconds (the
// total time the loop may spend waiting) and by the dispatch's own remaining
// budget — a wait that would consume the whole remainder would re-run an
// attempt with no time to do anything, so the loop stops instead.
func (p retryPolicy) backoffFor(waited, remaining time.Duration, timed bool) (time.Duration, bool) {
	wait := p.backoff
	if p.maxElapsed > 0 {
		left := p.maxElapsed - waited
		if left <= 0 {
			return 0, false
		}
		if wait > left {
			wait = left
		}
	}
	if timed && wait >= remaining {
		return 0, false
	}
	return wait, true
}

// attemptIDFor names one attempt inside the dispatch's own id namespace: the id
// a single-attempt envelope already reports, suffixed from the second attempt on
// so a retried dispatch's ids are unique and ordered. It returns "" when the
// lane minted no id to name an attempt with.
func attemptIDFor(res DispatchResult, attempt int) string {
	base := res.Meta.DispatchID
	if base == "" && res.Proposal != nil {
		base = res.Proposal.ProposalID
	}
	if base == "" {
		return ""
	}
	if attempt > 1 {
		return fmt.Sprintf("%s:attempt-%d", base, attempt)
	}
	return base
}

// runAttempts runs one attempt at a time until the policy stops, and returns the
// final attempt's result with the attempt bookkeeping aggregated onto it.
//
// Each attempt goes through finishDispatch, so every attempt records its own
// `yaah.subagent.dispatch_attempt.v1` fact: a retried dispatch's history is
// legible instead of only its last attempt. The aggregation on the returned
// result names every attempt in order (AttemptIDs), how many ran (Attempts), and
// therefore Retried — and the whole sequence's wall clock, not just the last
// attempt's.
//
// The total timeout is the dispatch budget for the entire sequence: attempt 1
// gets it whole, and each retry gets what is left of it. max_iterations and
// max_turns are per attempt (they travel in the params, unmodified), and usage
// aggregates across attempts because each attempt runs the same runner against
// the same parent accounting.
func runAttempts(
	ctx context.Context,
	req DispatchRequest,
	lane Lane,
	deadline time.Time,
	policy retryPolicy,
	attempt func(ctx context.Context, timeout time.Duration, n int) DispatchResult,
) DispatchResult {
	timed := !deadline.IsZero()
	startedAt := time.Now()

	var (
		ids    []string
		waited time.Duration
		final  DispatchResult
	)
	for n := 1; ; n++ {
		timeout := req.Timeout
		if n > 1 {
			// Each retry gets what is left of the total budget rather than a
			// fresh copy of it; attempt 1 keeps the exact configured value so a
			// single-attempt dispatch is unchanged to the byte.
			timeout = remainingBudget(deadline, timed)
		}

		res := attempt(ctx, timeout, n)
		if id := attemptIDFor(res, n); id != "" {
			ids = append(ids, id)
		}
		final = finishDispatch(ctx, req, res.withAttempts(n, ids))

		class := final.Envelope().FailureClass
		remaining := remainingBudget(deadline, timed)
		if !policy.shouldRetry(lane, class, n, remaining, timed) {
			break
		}
		wait, ok := policy.backoffFor(waited, remaining, timed)
		if !ok {
			break
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return final
			case <-timer.C:
			}
			waited += wait
		}
	}
	if final.Attempt > 1 {
		final = final.aggregate(time.Since(startedAt))
	}
	return final
}

// remainingBudget is the time left in the dispatch's total budget, or 0 when
// that budget is spent. An untimed dispatch reports 0 and every caller consults
// `timed` first, so "no budget" and "no time left" never blur.
func remainingBudget(deadline time.Time, timed bool) time.Duration {
	if !timed {
		return 0
	}
	return time.Until(deadline)
}
