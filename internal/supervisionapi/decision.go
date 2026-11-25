// Decision types for the supervisor policy loop (yaah-k0x1, design §4.5):
// one decider call per tick returns a LIST of per-agent decisions, and
// unlisted scopes are treated as none. This file is the vocabulary only —
// no action validation, no drop logic, and no truncation live here.
// Enforcing the word limits is the arbiter's job (yaah-3l34) and the
// headless decider's (yaah-0nbb); both count with WordCount.
package supervisionapi

import (
	"context"
	"strings"
)

// Action is one decision's verb. The string values are the wire form the
// supervise tool accepts and the envelope records.
type Action string

const (
	// ActionNone leaves the scope alone this tick. It is what an unlisted
	// scope means, so a decider rarely emits it explicitly.
	ActionNone Action = "none"
	// ActionSteer injects guidance the scope's run consumes between turns.
	ActionSteer Action = "steer"
	// ActionRedirect claims the scope for a live-redirect handoff: the
	// run is stopped and a successor starts on the same scope with the
	// decision's guidance as its first message.
	ActionRedirect Action = "redirect"
	// ActionRevert is a redirect whose adoption first rolls the scope's
	// workspace back to its pre-run snapshot.
	ActionRevert Action = "revert"
	// ActionStop is the ARBITER's rules-only action: an advisory rule may
	// stop a scope (design §4.6 precedence stop > revert > redirect >
	// steer), while the policy deliberately has no stop verb — a decider
	// may not end a worker's run. It never appears in a Decision and is
	// never part of the action mask, which governs the policy's actions;
	// a Decision that carries it is dropped as unknown_action. Exported
	// because the tick's records and the dispatch envelope (yaah-vaxv)
	// name it.
	ActionStop Action = "stop"
)

// The decision's word limits, in words as WordCount counts them.
const (
	// MaxReasonWords caps a decision's reason.
	MaxReasonWords = 20
	// MaxGuidanceWords caps the guidance of a steer, redirect, or revert.
	MaxGuidanceWords = 60
)

// Decision is one line of a per-agent decision list. At most one decision
// per scope per tick: the decider's list names scopes, and a scope named
// twice is a bug in the decider, not two interventions.
type Decision struct {
	// Scope is the scope label exactly as the snapshot renders it.
	Scope string
	// Action is none, steer, redirect, or revert — never stop, which is the
	// rules engine's verb alone (ActionStop).
	Action Action
	// Reason is the decision's justification, at most MaxReasonWords words.
	Reason string
	// Guidance is the injected or handed-off message, at most
	// MaxGuidanceWords words; required for steer, redirect and revert.
	Guidance string
}

// Decider turns one snapshot into one list of decisions. Stateless: one
// call, one list — the batch cadence lives with the caller, not here.
type Decider interface {
	Decide(ctx context.Context, snap Snapshot) ([]Decision, error)
}

// WordCount is the single rule for the word limits above: it counts
// whitespace-separated tokens (unicode whitespace), so an empty or
// all-space string is 0.
func WordCount(s string) int {
	return len(strings.Fields(s))
}
