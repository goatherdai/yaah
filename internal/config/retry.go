package config

import "time"

// retry.go is the configuration half of the dispatch-boundary retry policy
// (yaah-dgkh, decision yaah-hxk3.5). It carries only the numbers; the policy
// itself — which lanes and failure classes may be re-run, and how the dispatch
// budget is split across attempts — lives with the dispatch core
// (internal/dispatch/retry.go), because the lane matrix is a property of the
// dispatch, not of the config file.
//
// The zero value is the historical behavior: one attempt, no retry. That is
// what makes an absent `agents.subagent.retry` stanza byte-for-byte equivalent
// to a build that never had one.

// RetryConfig is agents.subagent.retry (and its per-role override
// agents.subagent.roles.<role>.retry).
type RetryConfig struct {
	// MaxAttempts bounds the total attempts for one dispatch, the first
	// attempt included. 1 (the default) means no retry, and 0 means the same
	// thing — the decision doc spells "off" as 0, so both readings are honored
	// rather than one of them turning into an unbounded loop. Negative values
	// are refused by validation.
	MaxAttempts int `yaml:"max_attempts"`

	// BackoffSecs is how long to wait before re-running an attempt. Seconds.
	// 0 re-runs immediately.
	BackoffSecs int `yaml:"backoff_secs"`

	// MaxElapsedSeconds bounds the total time the retry loop may spend backing
	// off between attempts. 0 means no separate bound (the dispatch's own
	// wall-clock timeout still governs every attempt). It caps the waiting, not
	// the attempts: an attempt is never granted time this budget withheld.
	MaxElapsedSeconds int `yaml:"max_elapsed_seconds"`

	// Only names the failure classes eligible for a retry, e.g.
	// [preflight_transient, timeout]. Empty means every retry candidate (the
	// first four classes of the dispatch taxonomy). A class that is not a retry
	// candidate is never eligible however it is listed — see
	// dispatch.RetryCandidate.
	Only []string `yaml:"only"`
}

// Attempts is MaxAttempts with its default applied: always at least one
// attempt, so an absent or empty retry stanza keeps the single-attempt
// dispatch.
func (r RetryConfig) Attempts() int {
	if r.MaxAttempts < 1 {
		return 1
	}
	return r.MaxAttempts
}

// Enabled reports whether the policy may re-run an attempt at all.
func (r RetryConfig) Enabled() bool { return r.Attempts() > 1 }

// Backoff is BackoffSecs as a duration.
func (r RetryConfig) Backoff() time.Duration {
	return time.Duration(r.BackoffSecs) * time.Second
}

// MaxElapsed is MaxElapsedSeconds as a duration; 0 means unbounded.
func (r RetryConfig) MaxElapsed() time.Duration {
	if r.MaxElapsedSeconds <= 0 {
		return 0
	}
	return time.Duration(r.MaxElapsedSeconds) * time.Second
}
