// Package supervisionapi holds the small vocabulary the supervisor tool and
// the agent's supervision service share, without either importing the other.
// The error sentinels keep errors.Is identity across the boundary; Firing is
// the read-only summary the status view renders.
package supervisionapi

import "errors"

var (
	// ErrNoRun means no live loop is registered for the scope.
	ErrNoRun = errors.New("no running loop registered for this scope")
	// ErrSteerBusy means the run's steering buffer is full.
	ErrSteerBusy = errors.New("the run's steering buffer is full; it is not consuming guidance")
	// ErrNotHaltable means the run registered no cancellation.
	ErrNotHaltable = errors.New("the run registered no cancellation; it is not haltable")
)

// Firing is one aggregated advisory-rule firing.
type Firing struct {
	Rule  string
	Scope string
	Kind  string
	Count int

	// Command and Reason carry a blocking deny's detail: the shell
	// command the guard refused and the reason it gave the model.
	// Both are empty for advisory firings, which steer rather than
	// refuse. A deny aggregates per command and reason, so the same
	// refused command counted twice is one line and two different
	// commands stay two — the count never hides what was refused.
	Command string
	Reason  string
}
