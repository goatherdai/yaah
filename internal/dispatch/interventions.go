package dispatch

// interventions.go renders the supervision interventions a sub-agent run was
// subject to as envelope caveats (yaah-vaxv, design §4.10 "Record"). It is the
// record half of the same route the guard denials take (denials.go): the trace
// already holds every intervention as a fact, and the orchestrator reads the
// envelope — so a run whose supervisor steered it, redirected it, or dropped an
// action against it must say so before its proposal is settled.
//
// The records arrive in tick order (the supervision log orders them), and the
// list is BOUNDED: a supervise loop that ticks for a whole dispatch can produce
// one record per scope per tick, and an envelope is not the place for that
// transcript. Past the budget one line states how many more there are; the trace
// keeps them all.

import (
	"fmt"
	"strings"

	"github.com/goatherdai/yaah/internal/jobs"
)

// interventionWarningBudget caps how many records one envelope names. Same shape
// as denialWarningBudget and the supervise tool's maxErrRecordDetails: the first
// records in tick order are named, and the rest are counted.
const interventionWarningBudget = 5

// interventionFieldClip bounds one record's reason or guidance. Both are
// model- or rule-authored text that must not be able to dominate the envelope.
const interventionFieldClip = 240

// interventionWarnings renders one run's intervention records as envelope
// warnings, one line per record in tick order and a trailing count when there
// are more than the budget. Each line names the source (policy, rules, root),
// the action, when it happened, what became of it, why it was dropped, and the
// guidance if there was any — so a reader learns what the supervisor did to the
// run and not merely that it did something.
func interventionWarnings(records []jobs.SubAgentIntervention) []string {
	if len(records) == 0 {
		return nil
	}
	out := make([]string, 0, len(records)+1)
	for i, rec := range records {
		if i == interventionWarningBudget {
			out = append(out, fmt.Sprintf("supervisor recorded %d more intervention(s) on this run", len(records)-interventionWarningBudget))
			break
		}
		out = append(out, interventionWarning(rec))
	}
	return out
}

// interventionWarning renders one record.
func interventionWarning(rec jobs.SubAgentIntervention) string {
	when := fmt.Sprintf("at tick %d", rec.Tick)
	if rec.Tick == 0 {
		// Tick 0 is "not from a tick": a rules action delivered directly, a
		// firing the full rule queue discarded, or the dispatching agent's own
		// root action through the supervisor tool.
		when = "outside the tick loop"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "supervisor %s %s %s: %s", rec.Source, rec.Action, when, rec.Outcome)
	if rec.DropReason != "" {
		// The reason is a stable snake_case token (supervisionapi), so it reads
		// as the answer to "why not" rather than as prose.
		fmt.Fprintf(&b, " (%s)", rec.DropReason)
	}
	// The reason is model-authored text on the policy path: one line only, so it
	// cannot forge a second warning entry.
	if reason := oneLine(rec.Reason, interventionFieldClip); reason != "" {
		b.WriteString(" — ")
		b.WriteString(reason)
	}
	if guidance := oneLine(rec.Guidance, interventionFieldClip); guidance != "" {
		fmt.Fprintf(&b, "; guidance %q", guidance)
	}
	if rec.Generation != 0 {
		fmt.Fprintf(&b, " (generation %d)", rec.Generation)
	}
	return b.String()
}
