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

// SetRetryPolicy installs the configured dispatch-boundary retry policy on a
// retained runtime: the global stanza, plus the per-role override callback. An
// override replaces the whole policy rather than merging into it, so a role can
// switch retry off under a global `max_attempts: 3`. Every composition root
// that builds a retained runtime for dispatched sub-agents calls this, so a
// workflow run retries under the same policy a session turn does.
func (rt *RetainedRuntime) SetRetryPolicy(sub config.SubAgentConfig) {
	rt.Retry = sub.Retry
	rt.RetryForRole = func(role string) (config.RetryConfig, bool) {
		if rc, ok := sub.Roles[role]; ok && rc.Retry != nil {
			return *rc.Retry, true
		}
		return config.RetryConfig{}, false
	}
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
// how many attempts have run so far (1 for the first) and terms carries what
// that attempt's own outcome leaves the policy: the time left in the dispatch's
// total budget, and whether the world it ran in can be discarded with it.
func (p retryPolicy) shouldRetry(lane Lane, class string, attempts int, terms attemptTerms) bool {
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
	// A world the attempt already ran in cannot be reset, so a re-run would
	// repeat its writes rather than start from the same place (yaah-dgkh
	// review).
	if terms.mutableWorld {
		return false
	}
	if terms.timed && terms.remaining <= 0 {
		return false
	}
	return true
}

// attemptTerms is what one finished attempt's own outcome leaves the retry
// policy: how much of the dispatch budget is left, and whether the world the
// attempt ran in survives it.
type attemptTerms struct {
	// remaining is the time left in the dispatch's total budget, and timed says
	// whether that budget exists at all (a dispatch with no timeout always
	// "has time").
	remaining time.Duration
	timed     bool
	// mutableWorld reports that the attempt ran in a world a retry cannot
	// reset: the session container a degraded report dispatch falls back to.
	// That world outlives the attempt and keeps whatever the attempt wrote.
	mutableWorld bool
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
// legible instead of only its last attempt. The fact is written after the
// decision, so it records what the policy actually allowed — not what the
// failure class alone would have allowed. The aggregation on the returned result
// names every attempt in order (AttemptIDs), how many ran (Attempts), and
// therefore Retried — and the whole sequence's wall clock, not just the last
// attempt's.
//
// The total timeout is the dispatch budget for the entire sequence, split across
// the attempts the policy permits: a timed-out attempt must leave budget for the
// retries the policy promises, or the `timeout` class could never be retried at
// all. max_iterations and max_turns are per attempt (they travel in the params,
// unmodified), and usage aggregates across attempts because each attempt runs the
// same runner against the same parent accounting.
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
		ids      []string
		warnings []string
		waited   time.Duration
		final    DispatchResult
	)
loop:
	for n := 1; ; n++ {
		// Before launching any further attempt: a parent that has gone away
		// takes its dispatch with it, whatever the policy decided. With a zero
		// backoff there is no wait to observe ctx.Done() in, so this is the
		// only check standing between a cancelled parent and another attempt
		// (yaah-dgkh review).
		if n > 1 && ctx.Err() != nil {
			break
		}
		timeout := attemptAllowance(req.Timeout, remainingBudget(deadline, timed), timed, n, policy.maxAttempts)

		res := attempt(ctx, timeout, n)
		if id := attemptIDFor(res, n); id != "" {
			ids = append(ids, id)
		}
		// A failed attempt's caveats ride forward: a worktree a failed attempt
		// stranded is still stranded after the retry succeeds (yaah-dgkh
		// review).
		warnings = append(warnings, res.Warnings...)
		res = res.withAttempts(n, ids).withWarnings(warnings)

		class := res.Envelope().FailureClass
		remaining := remainingBudget(deadline, timed)
		// The retry decision, resolved once and recorded: a cancelled parent
		// must not be read — or recorded — as a retryable attempt.
		next := ctx.Err() == nil && policy.shouldRetry(lane, class, n, attemptTerms{
			remaining:    remaining,
			timed:        timed,
			mutableWorld: res.MutableWorld,
		})
		var wait time.Duration
		if next {
			var ok bool
			if wait, ok = policy.backoffFor(waited, remaining, timed); !ok {
				next = false
			}
		}
		res.RetryAllowed = next
		final = finishDispatch(ctx, req, res)
		if !next {
			break
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				break loop
			case <-timer.C:
			}
		}
		waited += wait
	}
	if final.Attempt > 1 {
		// Every exit — the policy stopping, the budget running out, the parent
		// cancelling mid-backoff — reports the whole sequence's wall clock, not
		// the last attempt's span (yaah-dgkh review).
		final = final.aggregate(time.Since(startedAt))
	}
	return final
}

// attemptAllowance is one attempt's slice of the dispatch budget. With no retry
// configured the first and only attempt gets the whole budget, so a
// single-attempt dispatch behaves exactly as it did before the loop existed.
//
// With retry enabled the attempt gets its share of what is left, divided over the
// attempts still to run: attempt 1 of 3 gets a third, attempt 2 a half of the
// remainder, and the last attempt gets everything that is left. Splitting the
// budget is what makes `timeout` a retryable class at all — handing attempt 1 the
// whole budget leaves nothing for the retry the policy promised (yaah-dgkh
// review).
func attemptAllowance(total, remaining time.Duration, timed bool, attempt, maxAttempts int) time.Duration {
	if !timed || maxAttempts < 2 {
		return total
	}
	left := maxAttempts - attempt + 1
	if left < 1 {
		left = 1
	}
	share := remaining / time.Duration(left)
	if share < 0 {
		return 0
	}
	return share
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
