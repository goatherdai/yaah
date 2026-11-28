package agent

import (
	"context"
	"fmt"
)

// toolcall.go — the identity a tool call carries through the event stream.
//
// A fresh Loop is built per prompt (cmd/yaah/build_loop.go, runPrompt), so
// the loop's per-run dispatch ordinal restarts at 1 on every prompt: two
// prompts of one session emit tool calls carrying the same numbers. A view
// that treats that number as identity settles the wrong block (yaah-77f1),
// so the identity a tool-call event carries is composed here, once, and
// views read it off the event instead of deriving one.

// toolCallScopeKey is the context key carrying the interaction identity a
// tool call key is composed from.
type toolCallScopeKey struct{}

// withToolCallScope stamps the interaction identity onto ctx. Run calls it
// once per prompt; every tool dispatch of that run inherits it through the
// turn context.
func withToolCallScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, toolCallScopeKey{}, scope)
}

// toolCallKey returns the identity stamped on a tool call's start and end
// events. It is unique for the whole session: the scope is the interaction
// ID (newTurnID — random per Run, and the same cross-link the persisted
// messages and the trace turn facts carry), so an ordinal reused by a later
// run cannot collide with an earlier run's key.
//
// The identity this should carry is the persisted record's own — its owner
// plus path ordinal in the trace store, i.e. its content digest. That cannot
// be reached from here in this change: the trace middleware writes a tool
// call's declaration and capture in PostTool, which runs after the whole
// batch of calls has returned (internal/agent/turn.go), so at emit time the
// record does not exist yet. This function is therefore the seam: swapping
// its body for the persisted owner+ordinal changes no view.
func toolCallKey(ctx context.Context, ordinal int64) string {
	scope, _ := ctx.Value(toolCallScopeKey{}).(string)
	return fmt.Sprintf("%s.%d", scope, ordinal)
}
