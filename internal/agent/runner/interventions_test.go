package runner

// interventions_test.go pins the envelope route for supervision interventions
// (yaah-vaxv, design §4.10) the way guard_denials_test.go pins the denial route:
// the runner drains its own scope's records from the session's intervention log
// at settle and hands them to the jobs sink, order and all, exactly once.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// interventionRec builds one record for the fake supervisor to have made.
func interventionRec(tick int, action supervisionapi.Action, outcome, dropReason string) supervisionapi.InterventionRecord {
	return supervisionapi.InterventionRecord{
		Tick:       tick,
		Source:     supervisionapi.SourcePolicy,
		Action:     action,
		Reason:     "slow progress",
		Outcome:    outcome,
		DropReason: dropReason,
	}
}

// recordingInterventionProvider answers every model call with a report, and on
// its FIRST call records the interventions the supervisor made on the run. The
// bridge is registered before the loop's first model call, so the registry
// already names this dispatch's scope; recording from inside the run is what
// makes the test exercise the real drain at settle instead of a pre-seeded log.
//
// The records are appended OUT of tick order on purpose: the log, not the
// producer, is what orders them.
type recordingInterventionProvider struct {
	runs        *supervision.Registry
	recs        []supervisionapi.InterventionRecord
	calls       atomic.Int32
	recordScope string
}

func (p *recordingInterventionProvider) Send(context.Context, types.ChatRequest) (*types.ChatResponse, error) {
	if p.calls.Add(1) == 1 {
		if ids := p.runs.ScopeIDs(); len(ids) > 0 {
			p.recordScope = ids[0]
			// Stamp the registered scope onto copies: the log is keyed by
			// record.Scope and the runner drains by the bridge scope, so
			// records built without one would land under "" and the settle
			// would legitimately find nothing.
			recs := make([]supervisionapi.InterventionRecord, len(p.recs))
			copy(recs, p.recs)
			for i := range recs {
				recs[i].Scope = p.recordScope
			}
			p.runs.RecordInterventions(recs...)
		}
	}
	return &types.ChatResponse{
		Choices: []types.Choice{{
			Message:      types.Message{Role: "assistant", Content: "the work is done"},
			FinishReason: "stop",
		}},
		Usage: types.Usage{TotalTokens: 3},
	}, nil
}

// TestTaskRunner_CapturesInterventionsInTickOrder pins the whole runner half: a
// settling run reports the records its scope accumulated, drained in tick order,
// through the same ctx sink the guard refusals use.
func TestTaskRunner_CapturesInterventionsInTickOrder(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	mgr, store, root := newStartGateFixture(t, "sess-vaxv-capture", nil)
	provider := &recordingInterventionProvider{
		runs: runs,
		recs: []supervisionapi.InterventionRecord{
			interventionRec(3, supervisionapi.ActionSteer, supervisionapi.OutcomeDropped, supervisionapi.DropReasonCooldown),
			interventionRec(1, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
			interventionRec(2, supervisionapi.ActionRevert, supervisionapi.OutcomeDropped, supervisionapi.DropReasonNotReversible),
		},
	}

	var got []tools.SubAgentIntervention
	ctx := tools.WithSubAgentInterventions(context.Background(), &got)
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, false), 0)

	result, err := runner(ctx, "do the work", tools.SubAgentParams{Role: "developer"})
	if err != nil {
		t.Fatalf("run: %v (result %q)", err, result)
	}
	if provider.recordScope == "" {
		t.Fatal("the fake supervisor found no registered scope: the bridge never came up")
	}
	if len(got) != 3 {
		t.Fatalf("captured %d intervention records, want 3: %+v", len(got), got)
	}
	wantTicks := []int{1, 2, 3}
	for i, want := range wantTicks {
		if got[i].Tick != want {
			t.Fatalf("record %d has tick %d, want %d (order %v)", i, got[i].Tick, want, got)
		}
	}
	if got[1].DropReason != supervisionapi.DropReasonNotReversible {
		t.Errorf("record 1 drop reason = %q, want the revert's not_reversible", got[1].DropReason)
	}
	if got[0].Source != supervisionapi.SourcePolicy {
		t.Errorf("record 0 source = %q, want %q", got[0].Source, supervisionapi.SourcePolicy)
	}
}

// TestTaskRunner_DrainsInterventionLogAtSettle pins drain-once: the records the
// settling run reported are cleared from the log, so a later settle of the same
// scope — a redirect successor's, or a second dispatch's — reports its own run's
// interventions and not the earlier run's.
func TestTaskRunner_DrainsInterventionLogAtSettle(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	mgr, store, root := newStartGateFixture(t, "sess-vaxv-drain", nil)
	provider := &recordingInterventionProvider{
		runs: runs,
		recs: []supervisionapi.InterventionRecord{
			interventionRec(1, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		},
	}

	var got []tools.SubAgentIntervention
	ctx := tools.WithSubAgentInterventions(context.Background(), &got)
	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, false), 0)
	if _, err := runner(ctx, "do the work", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("captured %d records, want the run's one", len(got))
	}

	if left := runs.Interventions(provider.recordScope); left != nil {
		t.Errorf("the settled run left %d record(s) behind: %+v", len(left), left)
	}
	if second := runs.DrainInterventions(provider.recordScope); second != nil {
		t.Errorf("a second drain returned %+v, want nil", second)
	}
}

// TestTaskRunner_WithoutAnInterventionSinkStillRuns pins the additive contract:
// a dispatch that installs no intervention sink behaves exactly as before — the
// runner drains the (empty) log and nothing else changes.
func TestTaskRunner_WithoutAnInterventionSinkStillRuns(t *testing.T) {
	initTestRoles(t)
	runs := supervision.NewRegistry()
	mgr, store, root := newStartGateFixture(t, "sess-vaxv-nosink", nil)
	provider := &recordingInterventionProvider{
		runs: runs,
		recs: []supervisionapi.InterventionRecord{
			interventionRec(1, supervisionapi.ActionSteer, supervisionapi.OutcomeApplied, ""),
		},
	}

	runner := makeTaskRunner(startGateOpts(mgr, store, root.ID(), runs, provider, false), 0)
	if _, err := runner(context.Background(), "do the work", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run without an intervention sink: %v", err)
	}
	// Without this the test is vacuous: a bridge that never came up records
	// nothing, and "the log is empty" would then prove nothing about the drain
	// (PR #415 review finding 5).
	if provider.recordScope == "" {
		t.Fatal("the fake supervisor found no registered scope: the bridge never came up, so the empty log below is not evidence of a drain")
	}
	// Nothing captured the records, but the run still settled and the log was
	// still drained: a missing sink must not leave records to pile up.
	if left := runs.Interventions(provider.recordScope); left != nil {
		t.Errorf("log kept %d record(s) with no sink installed: %+v", len(left), left)
	}
}
