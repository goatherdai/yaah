package dispatch

// denials.go renders the supervisor-guard refusals a sub-agent run recorded as
// envelope caveats (yaah-938). The trace half of the same refusal — the guard's
// deny facts and the supervision status firings — is the operator's record; the
// orchestrator reads this envelope, so a task whose child was refused part of
// what it attempted must not read as an unqualified success.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/goatherdai/yaah/internal/jobs"
)

// denialWarningBudget caps how many refusals one envelope names. A model that
// keeps retrying a refused command is bounded here rather than allowed to fill
// the envelope's warning list with one line per turn.
const denialWarningBudget = 5

// denialFieldClip bounds one command or reason field. A single denial must not
// be able to dominate the envelope with a huge payload the model supplied.
const denialFieldClip = 240

// denialWarnings renders one run's guard refusals as envelope warnings, one per
// distinct refusal and a trailing count when there are more than the budget.
// The wording names the tool, the refused command, the refusing rule when it is
// a role allowlist, and the reason the model itself was given — so a reader
// knows both what was refused and why.
func denialWarnings(denials []jobs.SubAgentDenial) []string {
	if len(denials) == 0 {
		return nil
	}
	out := make([]string, 0, len(denials)+1)
	for i, d := range denials {
		if i == denialWarningBudget {
			out = append(out, fmt.Sprintf("supervisor guard refused %d more call(s) this run", len(denials)-denialWarningBudget))
			break
		}
		out = append(out, denialWarning(d))
	}
	return out
}

// denialWarning renders one refusal.
func denialWarning(d jobs.SubAgentDenial) string {
	target := d.Tool
	if target == "" {
		target = "a tool call"
	}
	if d.Command != "" {
		// A command is model-supplied text: keep it on one line and quoted so
		// embedded newlines cannot forge extra warning entries.
		target = fmt.Sprintf("%s (%q)", target, oneLine(d.Command, denialFieldClip))
	}
	reason := oneLine(d.Reason, denialFieldClip)
	if d.Rule != "" {
		return fmt.Sprintf("supervisor guard denied %s by %s: %s", target, d.Rule, reason)
	}
	return fmt.Sprintf("supervisor guard denied %s: %s", target, reason)
}

// oneLine collapses control characters to spaces and bounds the field length.
// It must NOT use substrate.TruncateAt: that helper's marker starts with a
// newline, which would reintroduce the very line break this function folds.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...[clipped]"
}
