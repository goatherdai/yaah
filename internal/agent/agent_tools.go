package agent

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/goatherdai/yaah/internal/agent/pipeline"
	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/types"
)

// executeAndCollect runs tool calls concurrently and returns ToolResult
// for middleware inspection. Each call's lifecycle is owned by a
// toolDispatch (agent_dispatch.go); this function only orchestrates the
// fan-out, result ordering, and conversation append.
func (l *Loop) executeAndCollect(ctx context.Context, calls []types.ToolCall, messages *[]types.Message) []pipeline.ToolResult {
	results := make([]pipeline.ToolResult, len(calls))
	ordered := make([]toolExecResult, len(calls))
	execResults := make(chan toolExecResult, len(calls))

	for i, tc := range calls {
		d := l.newDispatch(i, tc)
		go d.run(ctx, execResults)
	}

	for range len(calls) {
		r := <-execResults
		ordered[r.idx] = r
	}

	for i, r := range ordered {
		results[i] = pipeline.ToolResult{
			Name:     r.name,
			Args:     r.args,
			Result:   r.content,
			Error:    r.err,
			Duration: r.dur,
			CallID:   r.callID,
			Meta:     r.meta,
		}
		msg := types.ToolResultMsg(r.callID, r.name, r.content)
		// The persisted outcome is what the engine itself knows: a call that
		// returned a result is ok, a call that ended in an error is error.
		// Nothing here is written as cancelled or interrupted — a call
		// cancelled while queued arrives as an error, and a call that never
		// produced a result writes no row at all (replay derives interrupted
		// from that absence).
		msg.Outcome = toolOutcomeFromErr(r.err)
		msg.Duration = r.dur
		*messages = append(*messages, msg)
		l.Persister.Persist(msg)
	}

	return results
}

// toolOutcomeFromErr maps a dispatch's error onto the persisted outcome. The
// error is the only terminal signal the loop holds, so this is the whole
// derivation: no result text is inspected (a successful command may print
// "error: ") and no other status is inferable from here.
func toolOutcomeFromErr(err error) types.ToolOutcome {
	if err != nil {
		return types.ToolOutcomeError
	}
	return types.ToolOutcomeOK
}

// gateVerdictFailed determines whether a quality gate validator's
// output indicates failure. When the validator role's response contract
// declares a verdict field, the structured block is authoritative.
// Without it, the last-occurrence heuristic decides ("PASS" appearing
// after the last "FAIL" avoids false positives from test output that
// mentions both words) and a warning is logged so the role contract
// gets a verdict field (review B8).
func gateVerdictFailed(validatorRole, output string) bool {
	profile := subagent.RoleProfileFor(subagent.SubAgentRole(validatorRole))
	if !contractHasVerdict(profile.Contract) {
		slog.Warn("quality gate role lacks a verdict contract field; falling back to heuristic verdict",
			"role", validatorRole)
		return gateVerdictFail(output)
	}
	if v, ok := parseContractVerdict(output); ok {
		return strings.EqualFold(strings.TrimSpace(v), "FAIL")
	}
	// Contract present but no verdict block in the output — heuristic.
	return gateVerdictFail(output)
}

// contractHasVerdict reports whether the contract declares a field
// named "verdict" (case-insensitive).
func contractHasVerdict(c subagent.ContractDef) bool {
	if len(c.Fields) == 0 {
		return false
	}
	for _, f := range c.Fields {
		if strings.EqualFold(f.Name, "verdict") {
			return true
		}
	}
	return false
}

// verdictFieldRe matches a contract block bullet like
// "- **verdict**: PASS" / "- **verdict** (evidence): FAIL".
var verdictFieldRe = regexp.MustCompile(`(?im)^\s*[-*]\s*\*\*verdict\*\*(?:\s*\([^)]*\))?\s*[:\-–]\s*([A-Za-z]+)`)

// parseContractVerdict extracts the verdict value from a sub-agent's
// structured contract block. It scans for the verdict bullet anywhere
// after the contract heading when one is declared, or anywhere in the
// output otherwise.
func parseContractVerdict(output string) (string, bool) {
	m := verdictFieldRe.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// gateVerdictFail is the last-occurrence heuristic fallback for gate
// verdicts. Kept for roles whose contract does not (yet) declare a
// verdict field.
func gateVerdictFail(output string) bool {
	upper := strings.ToUpper(output)
	lastFail := strings.LastIndex(upper, "FAIL")
	if lastFail < 0 {
		return false
	}
	lastPass := strings.LastIndex(upper, "PASS")
	return lastPass < lastFail
}
