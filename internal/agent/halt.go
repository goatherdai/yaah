package agent

import (
	"context"
	"errors"
)

// ErrRunHalted is the cancellation cause a deliberate stop stamps onto the
// run context: the user's Esc/:stop through HaltCurrentRun or the stop
// hooks, and the supervisor's root-halt bridge. Runs cancelled with any
// other cause — parent deadline, session close, headless signal handling
// — are not halts and keep today's error reporting.
var ErrRunHalted = errors.New("run halted")

// HaltedContentSuffix is appended to a halted run's partial answer before
// it is sealed into the conversation. The marker is part of the message
// content, so the persisted transcript, the resumed view, and the next
// turn's model context all see that the answer was interrupted.
const HaltedContentSuffix = "\n\n[interrupted]"

// RunHalted reports whether ctx was cancelled by a deliberate stop. It
// reads the cancellation cause, not the error value, so a halt is still
// recognized after the cause propagates through derived contexts
// (compaction, sub-agents).
func RunHalted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRunHalted)
}
