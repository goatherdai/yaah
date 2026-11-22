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

// Firing is one aggregated rule firing: one per rule, scope, and kind, so
// the count is the aggregate and the map it is read from stays bounded.
type Firing struct {
	Rule  string
	Scope string
	Kind  string
	Count int

	// Command and Reason carry the MOST RECENT blocking deny's detail
	// for this rule, scope, and kind: the shell command the guard
	// refused and the reason it gave the model. Both are empty for
	// advisory firings, which steer rather than refuse, and for a deny
	// that carried no command. Both are bounded (the command is
	// model-authored text, so it is truncated before it is stored) and
	// both describe only the latest refusal — an earlier one is
	// summarised by the count, not by a line of its own.
	Command string
	Reason  string
}
