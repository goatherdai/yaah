package dispatch

// interventions_test.go pins the envelope rendering of supervision interventions
// (yaah-vaxv, design §4.10): the renderer's shape, its bound, the shared lane's
// route into the envelope's warnings, and — the additive half — that a run with
// no interventions renders exactly as it did before the record existed.

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// interventionRecord builds one record for the renderer.
func interventionRecord(tick int, source string, action supervisionapi.Action, outcome, dropReason, reason, guidance string) jobs.SubAgentIntervention {
	return jobs.SubAgentIntervention{
		Tick:       tick,
		Scope:      "scope:worker",
		Source:     source,
		Action:     action,
		Reason:     reason,
		Guidance:   guidance,
		Outcome:    outcome,
		DropReason: dropReason,
		Generation: 7,
	}
}

// interventionChild is a runner that reported these interventions during its run.
func interventionChild(recs ...jobs.SubAgentIntervention) TaskRunner {
	return func(ctx context.Context, _ string, _ SubAgentParams) (string, error) {
		jobs.CaptureSubAgentInterventions(ctx, recs)
		return "the report", nil
	}
}

// TestInterventionWarningsRendersEachRecord pins one line per record, naming the
// source, the action, when it happened, what became of it (and why, when it was
// dropped), and the guidance that was delivered.
func TestInterventionWarningsRendersEachRecord(t *testing.T) {
	recs := []jobs.SubAgentIntervention{
		interventionRecord(1, supervisionapi.SourcePolicy, supervisionapi.ActionSteer,
			supervisionapi.OutcomeApplied, "", "progress stalled", "narrow the diff"),
		interventionRecord(2, supervisionapi.SourcePolicy, supervisionapi.ActionRevert,
			supervisionapi.OutcomeDropped, supervisionapi.DropReasonNotReversible, "wrong turn", "start over"),
		interventionRecord(0, supervisionapi.SourceRoot, supervisionapi.ActionStop,
			supervisionapi.OutcomeApplied, "", "supervisor tool halt", ""),
	}

	got := interventionWarnings(recs)
	if len(got) != 3 {
		t.Fatalf("rendered %d lines, want one per record: %q", len(got), got)
	}

	want := []string{
		`supervisor policy steer at tick 1: applied — progress stalled; guidance "narrow the diff" (generation 7)`,
		`supervisor policy revert at tick 2: dropped (not_reversible) — wrong turn; guidance "start over" (generation 7)`,
		`supervisor root stop outside the tick loop: applied — supervisor tool halt (generation 7)`,
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("line %d = %q, want %q", i, got[i], w)
		}
	}

	if none := interventionWarnings(nil); none != nil {
		t.Errorf("interventionWarnings(nil) = %q, want nil", none)
	}
}

// TestInterventionWarningsAreBounded pins the cap: the first records in tick
// order are named and the rest are counted, so one long supervise loop cannot
// fill an envelope with its transcript.
func TestInterventionWarningsAreBounded(t *testing.T) {
	var recs []jobs.SubAgentIntervention
	for i := 1; i <= caveatWarningBudget+3; i++ {
		recs = append(recs, interventionRecord(i, supervisionapi.SourcePolicy, supervisionapi.ActionSteer,
			supervisionapi.OutcomeDropped, supervisionapi.DropReasonCooldown, "cooling", ""))
	}

	got := interventionWarnings(recs)
	if len(got) != caveatWarningBudget+1 {
		t.Fatalf("rendered %d lines, want %d named plus the count", len(got), caveatWarningBudget+1)
	}
	for i := 0; i < caveatWarningBudget; i++ {
		if !strings.Contains(got[i], "at tick ") {
			t.Errorf("line %d = %q, want a named record", i, got[i])
		}
	}
	if last := got[caveatWarningBudget]; !strings.Contains(last, "supervisor recorded 3 more intervention(s) on this run") {
		t.Errorf("last line = %q, want the count of the records past the budget", last)
	}
}

// TestRunSharedLaneReportsInterventions proves the route end to end: a run whose
// supervisor intervened carries the record into the envelope's warnings next to
// the guard refusals, in tick order, and the model-facing envelope renders it.
func TestRunSharedLaneReportsInterventions(t *testing.T) {
	recs := []jobs.SubAgentIntervention{
		interventionRecord(1, supervisionapi.SourcePolicy, supervisionapi.ActionSteer,
			supervisionapi.OutcomeApplied, "", "progress stalled", "narrow the diff"),
		interventionRecord(2, supervisionapi.SourceRules, supervisionapi.ActionStop,
			supervisionapi.OutcomeDropped, supervisionapi.DropReasonStopRefused, "stuck", ""),
	}
	res, err := Run(context.Background(), DispatchRequest{
		Lane:   LaneShared,
		Params: SubAgentParams{Role: "worker"},
		Prompt: "do the work",
		Runner: interventionChild(recs...),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		`supervisor policy steer at tick 1: applied — progress stalled; guidance "narrow the diff" (generation 7)`,
		`supervisor rules stop at tick 2: dropped (stop_refused) — stuck (generation 7)`,
	}
	if len(res.Warnings) != len(want) {
		t.Fatalf("warnings = %q, want the two intervention lines", res.Warnings)
	}
	for i, w := range want {
		if res.Warnings[i] != w {
			t.Errorf("warning %d = %q, want %q", i, res.Warnings[i], w)
		}
	}
	env := res.Envelope()
	if len(env.Warnings) != len(want) || !strings.Contains(env.Warnings[0], "supervisor policy steer at tick 1") {
		t.Errorf("envelope warnings = %q, want the interventions", env.Warnings)
	}
	if !strings.Contains(env.Warning, "supervisor policy steer") {
		t.Errorf("envelope warning = %q, want the first intervention in the scalar field too", env.Warning)
	}
}

// TestEnvelopeWithoutInterventionsIsUnchanged is the additive half: a run that
// recorded no interventions produces no intervention warning, so an existing
// consumer's envelope is byte-for-byte what it was before the record existed.
func TestEnvelopeWithoutInterventionsIsUnchanged(t *testing.T) {
	res, err := Run(context.Background(), DispatchRequest{
		Lane:   LaneShared,
		Params: SubAgentParams{Role: "worker"},
		Prompt: "do the work",
		Runner: func(context.Context, string, SubAgentParams) (string, error) { return "the report", nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", res.Warnings)
	}
	env := res.Envelope()
	if len(env.Warnings) != 0 || env.Warning != "" {
		t.Errorf("envelope warnings = %q / warning = %q, want both empty", env.Warnings, env.Warning)
	}
	if env.Result != "the report" {
		t.Errorf("result = %q, want the child's report untouched", env.Result)
	}
}
