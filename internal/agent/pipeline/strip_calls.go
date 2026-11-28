package pipeline

import "github.com/goatherdai/yaah/internal/types"

// stripDecision is one call's fate. A kept call has no reason; a stripped
// call carries the error text its synthesized result will use. An empty
// reason is still a strip — some gates deliberately say nothing.
type stripDecision struct {
	keep   bool
	reason string
}

// stripCalls is the single owner of the "remove a tool call and answer its
// id" mechanic shared by approval, inline_limit and supervisor_guard. It
// keeps calls for which decide returns keep, appends one error-outcome
// result per stripped call to step.SynthesizedResults, assigns the filtered
// list back to msg, and returns the number stripped (Step.InlineDropped).
//
// onStrip, when non-nil, fires once per stripped call before its result is
// appended — the approval middleware uses it to emit its denial hook.
func stripCalls(
	msg *types.Message,
	step *Step,
	decide func(i int, tc types.ToolCall) stripDecision,
	onStrip func(tc types.ToolCall, reason string),
) int {
	filtered := make([]types.ToolCall, 0, len(msg.ToolCalls))
	dropped := 0
	for i, tc := range msg.ToolCalls {
		d := decide(i, tc)
		if d.keep {
			filtered = append(filtered, tc)
			continue
		}
		dropped++
		if onStrip != nil {
			onStrip(tc, d.reason)
		}
		step.SynthesizedResults = append(step.SynthesizedResults,
			types.ToolFailedMsg(tc.ID, tc.Function.Name, d.reason))
	}
	msg.ToolCalls = filtered
	return dropped
}
