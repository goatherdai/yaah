package types

// ToolOutcome is a tool call's structured result: what happened, as opposed
// to the free-form text the tool returned.
//
// It exists so a consumer settling a tool block — the resumed transcript
// above all — decides from a status instead of pattern-matching the result
// text, which misclassifies a successful command whose output happens to
// start with "error: " (yaah-77f1, yaah-bmv3).
//
// The zero value means UNKNOWN, and unknown is NOT success: it is a row
// written before outcomes were persisted, or a call whose fate the engine
// never learned. Consumers keep their legacy text rule for exactly that case
// and never treat unknown as ok.
type ToolOutcome string

const (
	// ToolOutcomeUnknown is the zero value: no outcome was recorded. A
	// persisted row carrying it predates the outcome column.
	ToolOutcomeUnknown ToolOutcome = ""
	// ToolOutcomeOK is a call that ran to completion and returned a result.
	ToolOutcomeOK ToolOutcome = "ok"
	// ToolOutcomeError is a call that failed: the engine holds an error for
	// it and the result text is the flattened error.
	ToolOutcomeError ToolOutcome = "error"
	// ToolOutcomeCancelled is a call abandoned before it ran (work queued
	// and then cancelled). Reserved: nothing publishes it yet, so no row
	// carries it — the queue-cancel events land with the never-started
	// slice of yaah-77f1.
	ToolOutcomeCancelled ToolOutcome = "cancelled"
	// ToolOutcomeInterrupted is a call with no result: the turn ended before
	// a result existed for it. The resumed transcript derives it
	// structurally (a call row with no matching result row), because there
	// is no result to have carried the status.
	ToolOutcomeInterrupted ToolOutcome = "interrupted"
)

// Known reports whether an outcome was actually recorded. A caller deciding
// from status must treat !Known() as "no data" and fall back to its legacy
// rule rather than assume success.
func (o ToolOutcome) Known() bool { return o != ToolOutcomeUnknown }

// ParseToolOutcome maps a persisted outcome string back onto the type. An
// unrecognized string — a row written by a newer binary, or garbage — parses
// as ToolOutcomeUnknown rather than as a status nobody defined, so an
// unknown future value degrades to the legacy rule instead of being trusted.
func ParseToolOutcome(s string) ToolOutcome {
	switch o := ToolOutcome(s); o {
	case ToolOutcomeOK, ToolOutcomeError, ToolOutcomeCancelled, ToolOutcomeInterrupted:
		return o
	}
	return ToolOutcomeUnknown
}
