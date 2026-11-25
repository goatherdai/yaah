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

// The two envelope caveat lists — guard refusals and supervision interventions —
// are one shape: a bounded list of one-line, clipped entries and a trailing count.
// They share these constants and caveatLines so the two cannot drift into
// different budgets for the same reason, and a reader of one learns the other
// (PR #415 review finding 9).
const (
	// caveatWarningBudget caps how many entries one envelope names. A model
	// that keeps retrying a refused command, or a supervise loop that ticks for
	// a whole dispatch, is bounded here rather than allowed to fill the
	// envelope's warning list.
	caveatWarningBudget = 5
	// caveatFieldClip bounds one model- or rule-authored field in a caveat
	// line. A single entry must not be able to dominate the envelope with a
	// huge payload the model supplied.
	caveatFieldClip = 240
)

// caveatLines renders entries as at most caveatWarningBudget one-line entries in
// order, followed by one line stating how many were left out. Both caveat
// renderers go through it: same bound, same truncation, same count line shape.
func caveatLines[T any](entries []T, render func(T) string, overflow func(omitted int) string) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries)+1)
	for i, entry := range entries {
		if i == caveatWarningBudget {
			out = append(out, overflow(len(entries)-caveatWarningBudget))
			break
		}
		out = append(out, render(entry))
	}
	return out
}

// denialWarnings renders one run's guard refusals as envelope warnings, one per
// distinct refusal and a trailing count when there are more than the budget.
// The wording names the tool, the refused command, the refusing rule when it is
// a role allowlist, and the reason the model itself was given — so a reader
// knows both what was refused and why.
func denialWarnings(denials []jobs.SubAgentDenial) []string {
	return caveatLines(denials, denialWarning, func(omitted int) string {
		return fmt.Sprintf("supervisor guard refused %d more call(s) this run", omitted)
	})
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
		target = fmt.Sprintf("%s (%q)", target, oneLine(d.Command, caveatFieldClip))
	}
	reason := oneLine(d.Reason, caveatFieldClip)
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
