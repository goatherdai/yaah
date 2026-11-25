package supervision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	"github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// fakeSource is a SnapshotSource a test sets directly, so a batch's ingest is
// a controlled step rather than a read of a live registry.
type fakeSource struct {
	mu   sync.Mutex
	snap supervisionapi.Snapshot
}

func (f *fakeSource) set(agents ...supervisionapi.AgentSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = supervisionapi.Snapshot{At: testClock(), Agents: agents}
}

func (f *fakeSource) Snapshot() supervisionapi.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.snap
	out.Agents = append([]supervisionapi.AgentSnapshot(nil), f.snap.Agents...)
	return out
}

func testClock() time.Time { return time.Unix(1700000000, 0) }

// agentSnap is one live-or-finished agent as the snapshot builder would render
// it. Through starts at -1, the builder's "no facts observed" value.
func agentSnap(scopeID string, running bool) supervisionapi.AgentSnapshot {
	return supervisionapi.AgentSnapshot{ScopeID: scopeID, StillRunning: running, Through: -1}
}

// handoffSnap is a live agent whose registration may be claimed for a handoff
// (and, with capture set, whose pre-run workspace capture exists): the two
// facts a redirect or revert needs in the snapshot. A scope without them is
// dropped at the gauntlet, which is the root-protection boundary.
func handoffSnap(scopeID string, capture bool) supervisionapi.AgentSnapshot {
	a := agentSnap(scopeID, true)
	a.Redirectable = true
	a.Reversible = capture
	return a
}

// fakeRun is one registered run a test can inspect: the steering channel a
// delivery would land in, and how many times the handoff was asked to stop it.
type fakeRun struct {
	steer   chan string
	stopped int
}

// batchHarness assembles a batch over a fake snapshot source and a real
// registry, with an instant Wait and a recording StopFunc.
type batchHarness struct {
	t            *testing.T
	runs         *Registry
	src          *fakeSource
	batch        *Batch
	waits        int
	lastInterval time.Duration
	stops        []string
	stopReasons  []string
	// stopDelivered is the recording StopFunc's verdict and stopErr its
	// failure; the defaults are "delivered" and nil.
	stopDelivered bool
	stopErr       error
}

func newBatchHarness(t *testing.T, cfg BatchConfig) *batchHarness {
	return newBatchHarnessWithWait(t, cfg, nil)
}

func newBatchHarnessWithWait(t *testing.T, cfg BatchConfig, wait func(ctx context.Context, d time.Duration) error) *batchHarness {
	t.Helper()
	h := &batchHarness{t: t, runs: NewRegistry(), src: &fakeSource{}, stopDelivered: true}
	h.src.set()
	cfg.Now = testClock
	cfg.Stop = func(_ context.Context, scopeID, reason string) (bool, error) {
		h.stops = append(h.stops, scopeID)
		h.stopReasons = append(h.stopReasons, reason)
		return h.stopDelivered, h.stopErr
	}
	if wait != nil {
		cfg.Wait = wait
	} else {
		cfg.Wait = func(ctx context.Context, d time.Duration) error {
			h.waits++
			h.lastInterval = d
			return ctx.Err()
		}
	}
	h.batch = NewBatch(h.runs, h.src, cfg)
	return h
}

// run registers a run for the scope and returns its steering channel handle.
// The registration is redirectable and carries a handoff function, which is what
// the sub-agent runner supplies (Run.Redirectable); use runRootLike for the root
// shape, which is neither.
func (h *batchHarness) run(scopeID string) *fakeRun {
	r := &fakeRun{steer: make(chan string, SteerBuffer)}
	stop := func() { r.stopped++ }
	h.runs.Register(scopeID, Run{SteerCh: r.steer, Cancel: stop, Handoff: stop, Redirectable: true})
	return r
}

// runRootLike registers the shape the session root registers: a steering channel
// and a cancellation, no handoff, and Redirectable false. A redirect or revert
// targeting it must be dropped not_redirectable — that flag, not
// BatchConfig.RootScopeID, is the enforced boundary.
func (h *batchHarness) runRootLike(scopeID string) *fakeRun {
	r := &fakeRun{steer: make(chan string, SteerBuffer)}
	h.runs.Register(scopeID, Run{SteerCh: r.steer, Cancel: func() { r.stopped++ }})
	return r
}

// capture marks the scope's live registration reversible, the post-capture
// MarkReversible write the runner makes once the pre-run snapshot succeeded. The
// claim-time re-validation reads this flag, not the snapshot's.
func (h *batchHarness) capture(scopeID string) {
	h.t.Helper()
	gen, ok := h.runs.Generation(scopeID)
	if !ok {
		h.t.Fatalf("capture(%q): no live registration", scopeID)
	}
	if !h.runs.MarkReversible(scopeID, gen) {
		h.t.Fatalf("capture(%q): MarkReversible at generation %d did not land", scopeID, gen)
	}
}

func (h *batchHarness) render() string { return h.batch.SnapshotText(0) }

func (h *batchHarness) next(decisions ...supervisionapi.Decision) supervisionapi.TickResult {
	h.t.Helper()
	res, err := h.batch.Next(context.Background(), decisions)
	if err != nil {
		h.t.Fatalf("Next: %v", err)
	}
	return res
}

// pendingClaims reports how many claims are awaiting adoption.
func (h *batchHarness) pendingClaims() int {
	h.batch.mu.Lock()
	defer h.batch.mu.Unlock()
	return len(h.batch.claims)
}

// spent reports the batch's intervention budget usage.
func (h *batchHarness) spent() int {
	h.batch.mu.Lock()
	defer h.batch.mu.Unlock()
	return h.batch.interventions
}

// withoutStop clears the batch's stop delivery function — the no_stop_func
// configuration. Call it before the first tick: cfg is read without the lock,
// which is safe only while no tick is running.
func (h *batchHarness) withoutStop() { h.batch.cfg.Stop = nil }

// drainGuidance takes everything currently on a steering channel.
func drainGuidance(ch chan string) []string {
	var out []string
	for {
		select {
		case g := <-ch:
			out = append(out, g)
		default:
			return out
		}
	}
}

// words builds an n-word string with distinct words, for the word-limit tests.
func words(n int, prefix string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s%d", prefix, i+1)
	}
	return strings.Join(parts, " ")
}

// TestBatch_OneTickPerNext pins the step function's cadence: one Next call is
// one tick — one interval waited, one tick number consumed, and records stamped
// with the tick they ran on.
func TestBatch_OneTickPerNext(t *testing.T) {
	t.Run("explicit interval", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Interval: 2 * time.Second})
		h.src.set(agentSnap("scope:a", true))

		if got := h.batch.Tick(); got != 1 {
			t.Fatalf("tick before the first Next = %d, want 1", got)
		}
		res := h.next()
		if h.waits != 1 {
			t.Errorf("waits after one Next = %d, want exactly 1", h.waits)
		}
		if h.lastInterval != 2*time.Second {
			t.Errorf("waited %v, want the configured 2s", h.lastInterval)
		}
		if got := h.batch.Tick(); got != 2 {
			t.Errorf("tick after one Next = %d, want 2", got)
		}
		if res.Done {
			t.Error("Done with a live scope, want false")
		}

		res = h.next(supervisionapi.Decision{Scope: "w99", Action: supervisionapi.ActionSteer, Guidance: "x"})
		if h.waits != 2 {
			t.Errorf("waits after two Next calls = %d, want 2", h.waits)
		}
		if got := h.batch.Tick(); got != 3 {
			t.Errorf("tick after two Next calls = %d, want 3", got)
		}
		if len(res.Results) != 1 || res.Results[0].Tick != 2 {
			t.Fatalf("results = %+v, want one record stamped with tick 2", res.Results)
		}
	})

	t.Run("default interval", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.next()
		if h.lastInterval != DefaultBatchInterval {
			t.Errorf("waited %v, want the default %v", h.lastInterval, DefaultBatchInterval)
		}
	})
}

// TestBatch_StaleDecisionNeverApplies pins the anti-staleness guarantee a slow
// decider depends on: a decision naming a label from an older render resolves
// only against the labels and the snapshot that produced it, so a scope that
// left in the meantime is a recorded drop, never an intervention on a
// different worker.
func TestBatch_StaleDecisionNeverApplies(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
	a := h.run("scope:a")
	b := h.run("scope:b")

	text := h.render()
	if !strings.Contains(text, "TICK 1") {
		t.Fatalf("rendered snapshot has no tick header:\n%s", text)
	}
	if !strings.Contains(text, "## AGENT w1 (scope scope:a)") {
		t.Fatalf("label assignment unexpected:\n%s", text)
	}

	// Both workers finish, and two ticks pass with no fresh render — the slow
	// decider is still thinking about w1 and w2.
	h.src.set(agentSnap("scope:a", false), agentSnap("scope:b", false))
	h.next()
	h.next()

	res := h.next(
		supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "start over"},
		supervisionapi.Decision{Scope: "w2", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
		supervisionapi.Decision{Scope: "w9", Action: supervisionapi.ActionSteer, Guidance: "start over"},
	)
	if len(res.Applied) != 0 {
		t.Fatalf("applied = %+v, want none: every decision was made against a stale render", res.Applied)
	}
	if len(res.Results) != 3 {
		t.Fatalf("results = %d records, want 3 (every decision is recorded)", len(res.Results))
	}
	wantReasons := []string{
		supervisionapi.DropReasonNotLive,
		supervisionapi.DropReasonNotLive,
		supervisionapi.DropReasonStale,
	}
	for i, want := range wantReasons {
		if got := res.Results[i].DropReason; got != want {
			t.Errorf("results[%d] = %q (%s), want drop reason %q", i, res.Results[i].Scope, got, want)
		}
		if res.Results[i].Outcome != supervisionapi.OutcomeDropped {
			t.Errorf("results[%d] outcome = %q, want dropped", i, res.Results[i].Outcome)
		}
	}
	if got := drainGuidance(a.steer); len(got) != 0 {
		t.Errorf("stale decision delivered %q to the finished scope", got)
	}
	if got := drainGuidance(b.steer); len(got) != 0 {
		t.Errorf("stale decision delivered %q to the finished scope", got)
	}
	if a.stopped != 0 || b.stopped != 0 {
		t.Errorf("a stale decision stopped a run: a=%d b=%d", a.stopped, b.stopped)
	}
}

// TestBatch_DoneOnlyAfterEveryScopeFinished pins Done: false while any live
// non-root scope remains, true once every one has finished, and vacuously true
// when the batch has never seen a scope at all (fy28's wait:settled must not
// hang on an empty universe).
func TestBatch_DoneOnlyAfterEveryScopeFinished(t *testing.T) {
	t.Run("zero scopes", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		if res := h.next(); !res.Done {
			t.Error("Done with no scopes at all = false, want true (vacuously)")
		}
	})

	t.Run("live scope", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
		if res := h.next(); res.Done {
			t.Error("Done with two live scopes = true, want false")
		}
		h.src.set(agentSnap("scope:a", false), agentSnap("scope:b", true))
		if res := h.next(); res.Done {
			t.Error("Done with one live scope left = true, want false")
		}
	})

	t.Run("all finished", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(agentSnap("scope:a", true))
		if res := h.next(); res.Done {
			t.Fatal("Done with a live scope = true, want false")
		}
		h.src.set(agentSnap("scope:a", false))
		if res := h.next(); !res.Done {
			t.Error("Done after every scope finished = false, want true")
		}
	})

	t.Run("root scope is not a member", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{RootScopeID: "scope:root"})
		h.src.set(agentSnap("scope:root", true))
		if res := h.next(); !res.Done {
			t.Error("the root scope counted as a live batch member: Done = false, want true")
		}
	})
}

// TestBatch_Precedence pins the arbiter's order — rules stop > policy revert >
// redirect > steer — for one scope in one tick.
func TestBatch_Precedence(t *testing.T) {
	t.Run("rules stop beats a policy revert", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(handoffSnap("scope:a", true))
		h.run("scope:a")
		h.capture("scope:a")
		h.render()

		h.batch.QueueRuleStop("scope:a", "the rule says stop")
		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRevert, Reason: "undo the damage", Guidance: "redo it properly",
		})

		if len(res.Results) != 2 {
			t.Fatalf("results = %+v, want the rules record then the policy record", res.Results)
		}
		stop, revert := res.Results[0], res.Results[1]
		if stop.Source != supervisionapi.SourceRules || stop.Action != supervisionapi.ActionStop || stop.Outcome != supervisionapi.OutcomeApplied {
			t.Errorf("results[0] = %+v, want the rules stop applied first", stop)
		}
		if revert.Outcome != supervisionapi.OutcomeDropped || revert.DropReason != supervisionapi.DropReasonSuperseded {
			t.Errorf("results[1] = %+v, want the policy revert dropped as superseded", revert)
		}
		if len(res.Applied) != 1 || res.Applied[0].Action != supervisionapi.ActionStop {
			t.Errorf("applied = %+v, want only the rules stop", res.Applied)
		}
		if got := h.stops; len(got) != 1 || got[0] != "scope:a" {
			t.Errorf("stops delivered = %v, want one stop of scope:a", got)
		}
	})

	t.Run("revert beats redirect beats steer", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(handoffSnap("scope:a", true))
		run := h.run("scope:a")
		h.capture("scope:a")
		h.render()

		res := h.next(
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "keep going"},
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionRevert, Guidance: "undo and restart"},
		)
		if len(res.Results) != 3 {
			t.Fatalf("results = %+v, want one record per decision", res.Results)
		}
		if res.Results[0].DropReason != supervisionapi.DropReasonDuplicate ||
			res.Results[1].DropReason != supervisionapi.DropReasonDuplicate {
			t.Errorf("lower-precedence decisions = %q/%q, want duplicate",
				res.Results[0].DropReason, res.Results[1].DropReason)
		}
		if res.Results[2].Outcome != supervisionapi.OutcomeClaimed || res.Results[2].Action != supervisionapi.ActionRevert {
			t.Errorf("results[2] = %+v, want the revert claimed", res.Results[2])
		}
		if res.Results[2].Generation == 0 {
			t.Error("the claimed record carries no generation")
		}
		if len(res.Applied) != 1 {
			t.Errorf("applied = %+v, want exactly the revert", res.Applied)
		}
		if run.stopped != 1 {
			t.Errorf("handoff stops = %d, want 1 (the claimed generation was stopped)", run.stopped)
		}
		if got := drainGuidance(run.steer); len(got) != 0 {
			t.Errorf("a claim also steered the run: %q", got)
		}
	})

	t.Run("redirect beats steer", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(handoffSnap("scope:a", false))
		h.run("scope:a")
		h.render()

		res := h.next(
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "keep going"},
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
		)
		if res.Results[0].DropReason != supervisionapi.DropReasonDuplicate {
			t.Errorf("steer = %+v, want duplicate", res.Results[0])
		}
		if res.Results[1].Outcome != supervisionapi.OutcomeClaimed {
			t.Errorf("redirect = %+v, want claimed", res.Results[1])
		}
	})

	t.Run("one decision per scope across labels and ids", func(t *testing.T) {
		// The same scope named twice by different spellings is still one
		// scope: the winner map is keyed by the RESOLVED id.
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()

		res := h.next(
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "keep going"},
			supervisionapi.Decision{Scope: "scope:a", Action: supervisionapi.ActionSteer, Guidance: "keep going"},
		)
		if res.Results[0].Outcome != supervisionapi.OutcomeApplied {
			t.Errorf("results[0] = %+v, want the first decision applied", res.Results[0])
		}
		if res.Results[1].DropReason != supervisionapi.DropReasonDuplicate {
			t.Errorf("results[1] = %+v, want duplicate", res.Results[1])
		}
	})
}

// TestBatch_Cooldown pins the cooldown window: an acted scope ignores the next
// tick's action, the age advances one per tick, and an applied action resets it.
func TestBatch_Cooldown(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: -1}) // the documented default: one tick
	if got := h.batch.cfg.CooldownTicks; got != DefaultCooldownTicks {
		t.Fatalf("cooldown = %d, want the default %d", got, DefaultCooldownTicks)
	}
	h.src.set(agentSnap("scope:a", true))
	run := h.run("scope:a")
	h.render()

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "first"}); len(res.Applied) != 1 {
		t.Fatalf("first tick applied = %+v, want one steer", res.Applied)
	}
	text := h.render()
	if !strings.Contains(text, "ticks_ago=1") {
		t.Errorf("next render does not age the intervention to one tick:\n%s", text)
	}

	res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "second"})
	if len(res.Applied) != 0 || res.Results[0].DropReason != supervisionapi.DropReasonCooldown {
		t.Fatalf("next-tick steer = %+v, want dropped on cooldown", res.Results)
	}
	if text := h.render(); !strings.Contains(text, "ticks_ago=2") {
		t.Errorf("age did not advance to two ticks:\n%s", text)
	}

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "third"}); len(res.Applied) != 1 {
		t.Fatalf("steer after the window = %+v, want it applied (and the age reset)", res.Applied)
	}
	if text := h.render(); !strings.Contains(text, "ticks_ago=1") {
		t.Errorf("an applied action did not reset the age:\n%s", text)
	}
	got := drainGuidance(run.steer)
	if len(got) != 2 || got[0] != "first" || got[1] != "third" {
		t.Errorf("delivered = %q, want [first third]", got)
	}

	t.Run("disabled", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{CooldownTicks: 0})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()
		for i := 0; i < 2; i++ {
			res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "again"})
			if len(res.Applied) != 1 {
				t.Fatalf("cooldown 0 blocked tick %d: %+v", i+1, res.Results)
			}
		}
	})
}

// TestBatch_ActionMask pins supervisor.actions: an action outside the mask is
// dropped, and an explicitly empty mask denies every action.
func TestBatch_ActionMask(t *testing.T) {
	t.Run("steer only", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Actions: []supervisionapi.Action{supervisionapi.ActionSteer}})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()

		res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over"})
		if res.Results[0].DropReason != supervisionapi.DropReasonMaskedAction {
			t.Errorf("redirect under a steer-only mask = %+v, want masked_action", res.Results[0])
		}
	})

	t.Run("empty mask denies all", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Actions: []supervisionapi.Action{}})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()

		res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "keep going"})
		if res.Results[0].DropReason != supervisionapi.DropReasonMaskedAction {
			t.Errorf("steer under an empty mask = %+v, want masked_action", res.Results[0])
		}
	})

	t.Run("a masked action does not block an allowed one", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Actions: []supervisionapi.Action{supervisionapi.ActionSteer}})
		h.src.set(agentSnap("scope:a", true))
		run := h.run("scope:a")
		h.render()

		res := h.next(
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
			supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "keep going"},
		)
		if res.Results[0].DropReason != supervisionapi.DropReasonMaskedAction {
			t.Errorf("results[0] = %+v, want masked_action", res.Results[0])
		}
		if res.Results[1].Outcome != supervisionapi.OutcomeApplied {
			t.Errorf("results[1] = %+v, want the allowed steer applied (a masked action must not hold the scope)", res.Results[1])
		}
		if got := drainGuidance(run.steer); len(got) != 1 {
			t.Errorf("delivered = %q, want the steer", got)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()

		res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionStop, Guidance: "stop it"})
		if res.Results[0].DropReason != supervisionapi.DropReasonUnknownAction {
			t.Errorf("a policy stop = %+v, want unknown_action (the policy has no stop verb)", res.Results[0])
		}
	})

	t.Run("none is silent", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(agentSnap("scope:a", true))
		h.run("scope:a")
		h.render()

		res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionNone})
		if len(res.Results) != 0 {
			t.Errorf("results = %+v, want none: an unlisted scope and an explicit none are the same thing", res.Results)
		}
	})
}

// TestBatch_Cap pins max_interventions: cumulative across ticks, and 0 means
// unlimited.
func TestBatch_Cap(t *testing.T) {
	t.Run("cap reached", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{CooldownTicks: 0, MaxInterventions: 1})
		h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
		h.run("scope:a")
		h.run("scope:b")
		h.render()

		if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "first"}); len(res.Applied) != 1 {
			t.Fatalf("first tick applied = %+v, want one", res.Applied)
		}
		res := h.next(supervisionapi.Decision{Scope: "w2", Action: supervisionapi.ActionSteer, Guidance: "second"})
		if res.Results[0].DropReason != supervisionapi.DropReasonCapReached {
			t.Errorf("second intervention = %+v, want cap_reached", res.Results[0])
		}
	})

	t.Run("unlimited", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{CooldownTicks: 0, MaxInterventions: 0})
		h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
		h.run("scope:a")
		h.run("scope:b")
		h.render()

		for _, scope := range []string{"w1", "w2"} {
			res := h.next(supervisionapi.Decision{Scope: scope, Action: supervisionapi.ActionSteer, Guidance: "go"})
			if len(res.Applied) != 1 {
				t.Fatalf("%s under an unlimited cap = %+v, want applied", scope, res.Results)
			}
		}
	})
}

// TestBatch_RevertOnNonReversibleScope pins the one downgrade the arbiter
// refuses: a revert on a scope with no pre-run capture is dropped, never turned
// into a redirect — and nothing is claimed, cancelled, or steered on the way.
func TestBatch_RevertOnNonReversibleScope(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(handoffSnap("scope:a", false)) // no pre-run capture
	run := h.run("scope:a")
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRevert, Reason: "it broke the tree", Guidance: "undo and retry",
	})
	rec := res.Results[0]
	if rec.Outcome != supervisionapi.OutcomeDropped || rec.DropReason != supervisionapi.DropReasonNotReversible {
		t.Fatalf("revert on a non-reversible scope = %+v, want dropped/not_reversible", rec)
	}
	if rec.Action != supervisionapi.ActionRevert {
		t.Errorf("recorded action = %q, want the revert preserved (never rewritten to a redirect)", rec.Action)
	}
	if len(res.Applied) != 0 {
		t.Errorf("applied = %+v, want none", res.Applied)
	}
	if run.stopped != 0 {
		t.Errorf("the run was stopped (%d): a dropped revert must not cancel anything", run.stopped)
	}
	if got := drainGuidance(run.steer); len(got) != 0 {
		t.Errorf("the dropped revert steered the run: %q", got)
	}
	if n := h.pendingClaims(); n != 0 {
		t.Errorf("pending claims = %d, want 0", n)
	}
	// Proof by probe: the generation is still unclaimed, so a real redirect
	// from another supervisor would still win it.
	gen, ok := h.runs.Generation("scope:a")
	if !ok {
		t.Fatal("the run is gone: a dropped revert must leave it running")
	}
	if _, err := h.runs.Redirect("scope:a", gen, supervisionapi.RedirectRequest{Guidance: "probe"}); err != nil {
		t.Errorf("the generation is already claimed (%v): the dropped revert reached Redirect", err)
	}
}

// TestBatch_RulesSteerInsideCooldownSuppressesPolicy pins the shared arbiter's
// precedence: a rule steer inside the cooldown is dropped, and it still
// suppresses the policy's steer on the same scope in the same tick.
func TestBatch_RulesSteerInsideCooldownSuppressesPolicy(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: -1})
	h.src.set(agentSnap("scope:a", true))
	run := h.run("scope:a")
	h.render()

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "first"}); len(res.Applied) != 1 {
		t.Fatalf("first tick applied = %+v, want one", res.Applied)
	}
	drainGuidance(run.steer) // the first steer is not what this test asserts

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "policy says go"})

	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the rules record then the policy record", res.Results)
	}
	if res.Results[0].Source != supervisionapi.SourceRules || res.Results[0].DropReason != supervisionapi.DropReasonCooldown {
		t.Errorf("results[0] = %+v, want the rules steer dropped on cooldown", res.Results[0])
	}
	if res.Results[1].Source != supervisionapi.SourcePolicy || res.Results[1].DropReason != supervisionapi.DropReasonSuperseded {
		t.Errorf("results[1] = %+v, want the policy steer dropped as superseded", res.Results[1])
	}
	if len(res.Applied) != 0 {
		t.Errorf("applied = %+v, want none", res.Applied)
	}
	if got := drainGuidance(run.steer); len(got) != 0 {
		t.Errorf("delivered %q during the cooldown, want nothing", got)
	}
}

// TestBatch_RulesStopVoidsPendingClaim pins the interaction the design calls
// out: a claimed redirect does not survive a stop. The claim's record said
// "claimed" in the tick that made it, so the voiding tick reports it as dropped
// with the generation it targeted.
func TestBatch_RulesStopVoidsPendingClaim(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(handoffSnap("scope:a", false))
	run := h.run("scope:a")
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRedirect, Reason: "off track", Guidance: "restart on the parser",
	})
	if res.Results[0].Outcome != supervisionapi.OutcomeClaimed {
		t.Fatalf("redirect = %+v, want claimed", res.Results[0])
	}
	claimed := res.Results[0].Generation
	if h.pendingClaims() != 1 {
		t.Fatalf("pending claims = %d, want 1", h.pendingClaims())
	}

	h.batch.QueueRuleStop("scope:a", "the rule wins")
	res = h.next()
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the stop record then the void record", res.Results)
	}
	stop, void := res.Results[0], res.Results[1]
	if stop.Source != supervisionapi.SourceRules || stop.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[0] = %+v, want the rules stop applied", stop)
	}
	if void.Source != supervisionapi.SourcePolicy || void.Outcome != supervisionapi.OutcomeDropped {
		t.Errorf("results[1] = %+v, want the voided claim as a dropped policy record", void)
	}
	if void.DropReason != supervisionapi.DropReasonSupersededByStop {
		t.Errorf("void drop reason = %q, want superseded_by_stop", void.DropReason)
	}
	// The void record names the tick the claim was made in: the claimed record
	// lives in that earlier tick, and the void line has to stand on its own.
	if want := "claim from tick 1 superseded by rules stop"; void.Reason != want {
		t.Errorf("void reason = %q, want the claim's provenance %q", void.Reason, want)
	}
	if void.Generation != claimed {
		t.Errorf("void record generation = %d, want the claimed %d", void.Generation, claimed)
	}
	if void.Action != supervisionapi.ActionRedirect {
		t.Errorf("void record action = %q, want the redirect it voids", void.Action)
	}
	if h.pendingClaims() != 0 {
		t.Errorf("pending claims = %d after the stop, want 0", h.pendingClaims())
	}
	if got := h.stops; len(got) != 1 || got[0] != "scope:a" {
		t.Errorf("stops delivered = %v, want one stop of scope:a", got)
	}
	if run.stopped != 1 {
		t.Errorf("handoff stops = %d, want 1 (only the claim's own stop)", run.stopped)
	}
}

// TestBatch_LabelThreading pins the label contract: SnapshotText's labels are
// what Next resolves, and Known keeps a worker's name stable when a
// lower-sorting scope joins between ticks.
func TestBatch_LabelThreading(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
	a := h.run("scope:a")
	newcomer := h.run("scope:0a")

	text := h.render()
	if !strings.Contains(text, "## AGENT w1 (scope scope:a)") || !strings.Contains(text, "## AGENT w2 (scope scope:b)") {
		t.Fatalf("first render's labels are unexpected:\n%s", text)
	}
	h.next()

	// A scope sorting BELOW both joins; without Known its arrival would
	// renumber the others.
	h.src.set(agentSnap("scope:0a", true), agentSnap("scope:a", true), agentSnap("scope:b", true))
	h.next()
	text = h.render()
	if !strings.Contains(text, "## AGENT w1 (scope scope:a)") {
		t.Errorf("scope:a lost its label to the newcomer:\n%s", text)
	}
	if !strings.Contains(text, "## AGENT w3 (scope scope:0a)") {
		t.Errorf("the newcomer did not take the next free label:\n%s", text)
	}

	res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "for the original worker"})
	if len(res.Applied) != 1 {
		t.Fatalf("applied = %+v, want the steer applied", res.Applied)
	}
	if res.Results[0].Scope != "scope:a" {
		t.Errorf("recorded scope = %q, want scope:a (w1 resolved to the right worker)", res.Results[0].Scope)
	}
	if got := drainGuidance(a.steer); len(got) != 1 {
		t.Errorf("scope:a received %q, want the steer", got)
	}
	if got := drainGuidance(newcomer.steer); len(got) != 0 {
		t.Errorf("the newcomer received %q, want nothing", got)
	}
}

// TestBatch_WordLimits pins the arbiter's enforcement of the decision's word
// limits: whole words kept, the rest dropped, the truncated text recorded.
func TestBatch_WordLimits(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true))
	h.run("scope:a")
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope:    "w1",
		Action:   supervisionapi.ActionSteer,
		Reason:   words(supervisionapi.MaxReasonWords+1, "r"),
		Guidance: words(supervisionapi.MaxGuidanceWords+1, "g"),
	})
	rec := res.Results[0]
	if got := supervisionapi.WordCount(rec.Reason); got != supervisionapi.MaxReasonWords {
		t.Errorf("reason words = %d, want %d", got, supervisionapi.MaxReasonWords)
	}
	if got := supervisionapi.WordCount(rec.Guidance); got != supervisionapi.MaxGuidanceWords {
		t.Errorf("guidance words = %d, want %d", got, supervisionapi.MaxGuidanceWords)
	}
	if want := words(supervisionapi.MaxReasonWords, "r"); rec.Reason != want {
		t.Errorf("reason = %q, want the first %d words verbatim", rec.Reason, supervisionapi.MaxReasonWords)
	}
	if want := words(supervisionapi.MaxGuidanceWords, "g"); rec.Guidance != want {
		t.Errorf("guidance truncated to the wrong text: %q", rec.Guidance)
	}
	if rec.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("a truncated-but-valid steer = %+v, want applied", rec)
	}
}

// TestBatch_ConcurrentNextRefused pins the single-flight guard: a second tick
// while one is waiting is refused, not interleaved.
func TestBatch_ConcurrentNextRefused(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newBatchHarnessWithWait(t, BatchConfig{}, func(ctx context.Context, _ time.Duration) error {
		once.Do(func() { close(entered) })
		<-release
		return ctx.Err()
	})
	h.src.set(agentSnap("scope:a", true))

	done := make(chan error, 1)
	go func() {
		_, err := h.batch.Next(context.Background(), nil)
		done <- err
	}()
	<-entered

	if _, err := h.batch.Next(context.Background(), nil); !errors.Is(err, ErrBatchBusy) {
		t.Errorf("concurrent Next = %v, want ErrBatchBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Errorf("the first Next failed: %v", err)
	}
	// The guard is released: the next tick runs.
	if _, err := h.batch.Next(context.Background(), nil); err != nil {
		t.Errorf("Next after the tick finished = %v, want nil", err)
	}
}

// TestBatch_ClaimSurvivesUnstoppableRun pins the claim's durability across the
// stop: a claimed generation whose run cannot be stopped (or is already gone)
// keeps its "claimed" record — the runner's adoption loop owns the outcome, and
// a claim is real whether or not the cancellation landed.
//
// The ErrNoRun race itself — the run finishing between the claim and the
// cancel — cannot be scheduled deterministically without a registry seam, so
// the branch is pinned from both ends: cancelNotFatal over both sentinels, and
// this end-to-end claim whose stop fails with the sibling error.
func TestBatch_ClaimSurvivesUnstoppableRun(t *testing.T) {
	if !cancelNotFatal(supervisionapi.ErrNoRun) {
		t.Error("ErrNoRun after a claim must not fail the record")
	}
	if !cancelNotFatal(supervisionapi.ErrNotHaltable) {
		t.Error("ErrNotHaltable after a claim must not fail the record")
	}
	if cancelNotFatal(fmt.Errorf("registry exploded")) {
		t.Error("an unclassified stop failure must be reported")
	}

	h := newBatchHarness(t, BatchConfig{})
	h.src.set(handoffSnap("scope:a", false))
	// A run with a steering channel but no cancellation: the claim wins and
	// the stop cannot be delivered.
	h.runs.Register("scope:a", Run{SteerCh: make(chan string, SteerBuffer), Redirectable: true})
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRedirect, Reason: "off track", Guidance: "restart on the parser",
	})
	if res.Results[0].Outcome != supervisionapi.OutcomeClaimed {
		t.Fatalf("redirect whose stop failed = %+v, want still claimed", res.Results[0])
	}
	if len(res.Applied) != 1 {
		t.Errorf("applied = %+v, want the claim", res.Applied)
	}
}

// TestBatch_TriggerOnChange pins the caller hint:a quiet tick reports no
// change and asks the caller to skip its decider; an advanced cursor is a
// change. Decisions supplied anyway are still validated (the trigger never
// silently discards them).
func TestBatch_TriggerOnChange(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{Trigger: TriggerOnChange})
	h.src.set(agentSnap("scope:a", true))

	h.next()
	if h.batch.Changed() {
		t.Error("a quiet tick reported a change")
	}
	if h.batch.ShouldDecide() {
		t.Error("the on-change trigger asked for a decider after a quiet tick")
	}

	advanced := agentSnap("scope:a", true)
	advanced.Through = 4
	h.src.set(advanced)
	h.next()
	if !h.batch.Changed() {
		t.Error("an advanced Through cursor is not reported as a change")
	}
	if !h.batch.ShouldDecide() {
		t.Error("the on-change trigger withheld the decider after a change")
	}

	t.Run("errors count as change", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Trigger: TriggerOnChange})
		h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
		h.next()
		if h.batch.Changed() {
			t.Fatal("a quiet tick reported a change")
		}
		errored := agentSnap("scope:b", true)
		errored.ConsecutiveErrors = 2
		h.src.set(agentSnap("scope:a", true), errored)
		h.next()
		if !h.batch.Changed() {
			t.Error("a growing consecutive-error count is not reported as a change")
		}
	})

	t.Run("shared modified file counts as change", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Trigger: TriggerOnChange})
		one := agentSnap("scope:a", true)
		one.ModifiedFiles = []string{"internal/parse.go"}
		two := agentSnap("scope:b", true)
		two.ModifiedFiles = []string{"internal/parse.go"}
		h.src.set(one, two)
		h.next()
		if !h.batch.Changed() {
			t.Error("two scopes naming one file is not reported as a change")
		}
	})
}

// TestBatch_AlwaysTriggerDecides pins the default trigger: the caller is always
// asked for a decider while a live worker remains.
func TestBatch_AlwaysTriggerDecides(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true))
	h.next()
	if !h.batch.ShouldDecide() {
		t.Error("the always trigger withheld the decider")
	}
}

// TestBatch_RulesRankBeatsArrivalOrder pins the rules half's precedence: one
// rule action per scope per tick, chosen by ACTION rank and not by the order the
// Service's drain happened to queue them (design §4.6: stop > revert > redirect
// > steer). A steer queued first does not win.
func TestBatch_RulesRankBeatsArrivalOrder(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true))
	run := h.run("scope:a")
	h.render()

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	h.batch.QueueRuleStop("scope:a", "the rule says stop")
	res := h.next()

	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the outranked steer and the stop", res.Results)
	}
	if got := res.Results[0]; got.Action != supervisionapi.ActionSteer ||
		got.Outcome != supervisionapi.OutcomeDropped || got.DropReason != supervisionapi.DropReasonDuplicate {
		t.Errorf("results[0] = %+v, want the steer recorded as a duplicate", got)
	}
	if got := res.Results[1]; got.Action != supervisionapi.ActionStop || got.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[1] = %+v, want the stop applied", got)
	}
	if len(res.Applied) != 1 || res.Applied[0].Action != supervisionapi.ActionStop {
		t.Errorf("applied = %+v, want only the stop", res.Applied)
	}
	if got := drainGuidance(run.steer); len(got) != 0 {
		t.Errorf("the outranked steer was delivered anyway: %q", got)
	}
	if got := h.stops; len(got) != 1 || got[0] != "scope:a" {
		t.Errorf("stops delivered = %v, want one stop of scope:a", got)
	}
	if got := h.stopReasons; len(got) != 1 || got[0] != "the rule says stop" {
		t.Errorf("stop reasons = %v, want the queued reason forwarded to the delivery function", got)
	}
}

// TestBatch_RulesSteerDoesNotSuppressPolicyRevert pins the rank boundary between
// the tick's two halves: a rules steer outranks only a policy STEER, so a policy
// revert (and, the same mechanism, a redirect) on the same scope still reaches
// its gauntlet (design §4.6: stop > revert > redirect > steer).
func TestBatch_RulesSteerDoesNotSuppressPolicyRevert(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: 0})
	h.src.set(handoffSnap("scope:a", true))
	run := h.run("scope:a")
	h.capture("scope:a")
	h.render()

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRevert, Reason: "it broke the tree", Guidance: "undo and retry",
	})
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the rules steer then the policy revert", res.Results)
	}
	if got := res.Results[0]; got.Source != supervisionapi.SourceRules || got.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[0] = %+v, want the rules steer applied", got)
	}
	if got := res.Results[1]; got.Outcome != supervisionapi.OutcomeClaimed || got.Action != supervisionapi.ActionRevert {
		t.Errorf("results[1] = %+v, want the revert claimed: a rules steer must not suppress it", got)
	}
	if len(res.Applied) != 2 {
		t.Errorf("applied = %+v, want the steer and the claim", res.Applied)
	}
	if got := drainGuidance(run.steer); len(got) != 1 || got[0] != "try a different approach" {
		t.Errorf("delivered = %q, want the rules steer", got)
	}
	if run.stopped != 1 {
		t.Errorf("handoff stops = %d, want 1 (the claimed revert's own stop)", run.stopped)
	}
}

// TestBatch_StopNotDeliveredLeavesClaim pins the record-follows-effect rule: a
// claim is voided only by a stop that was actually DELIVERED. A missing delivery
// function or a failed one leaves the claim in the table — the runner will still
// adopt it — and records no false supersede.
func TestBatch_StopNotDeliveredLeavesClaim(t *testing.T) {
	// claimRedirect makes the pending claim this test then tries to void.
	claimRedirect := func(t *testing.T, h *batchHarness) uint64 {
		t.Helper()
		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRedirect, Reason: "off track", Guidance: "restart on the parser",
		})
		if res.Results[0].Outcome != supervisionapi.OutcomeClaimed {
			t.Fatalf("redirect = %+v, want claimed", res.Results[0])
		}
		if n := h.pendingClaims(); n != 1 {
			t.Fatalf("pending claims = %d, want 1", n)
		}
		return res.Results[0].Generation
	}
	setup := func(t *testing.T, h *batchHarness) {
		t.Helper()
		h.src.set(handoffSnap("scope:a", false))
		h.run("scope:a")
		h.render()
	}

	t.Run("no stop function", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		h.withoutStop()
		setup(t, h)
		claimRedirect(t, h)

		h.batch.QueueRuleStop("scope:a", "the rule says stop")
		res := h.next()
		if len(res.Results) != 1 {
			t.Fatalf("results = %+v, want only the stop's own drop: nothing was stopped, so nothing was voided", res.Results)
		}
		if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeDropped ||
			got.DropReason != supervisionapi.DropReasonNoStopFunc {
			t.Errorf("stop record = %+v, want dropped/no_stop_func", got)
		}
		if n := h.pendingClaims(); n != 1 {
			t.Errorf("pending claims = %d, want 1 (the runner still owns the claim)", n)
		}
	})

	t.Run("failed delivery, then a stop that lands", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{})
		setup(t, h)
		claimed := claimRedirect(t, h)

		h.stopErr = errors.New("the halt path exploded")
		h.batch.QueueRuleStop("scope:a", "boom")
		res := h.next()
		if len(res.Results) != 1 {
			t.Fatalf("results = %+v, want only the stop's own drop", res.Results)
		}
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonStopFailed {
			t.Errorf("stop record = %+v, want dropped/stop_failed", got)
		}
		if n := h.pendingClaims(); n != 1 {
			t.Errorf("pending claims = %d after a failed stop, want 1", n)
		}

		// The claim is still there, so the stop that finally lands voids it.
		h.stopErr = nil
		h.batch.QueueRuleStop("scope:a", "the rule wins")
		res = h.next()
		if len(res.Results) != 2 {
			t.Fatalf("results = %+v, want the stop record then the void record", res.Results)
		}
		if got := res.Results[1]; got.DropReason != supervisionapi.DropReasonSupersededByStop ||
			got.Generation != claimed || got.Action != supervisionapi.ActionRedirect {
			t.Errorf("void record = %+v, want the earlier claim voided at generation %d", got, claimed)
		}
		if n := h.pendingClaims(); n != 0 {
			t.Errorf("pending claims = %d after the stop landed, want 0", n)
		}
	})
}

// TestBatch_StopDoesNotVoidAdoptedClaim pins the generation check: between the
// claim and the stop landing, the runner may have adopted it and registered a
// successor. Voiding then would report a claim nobody stopped — and the stop
// itself halted the successor, which the claim's record does not describe.
func TestBatch_StopDoesNotVoidAdoptedClaim(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(handoffSnap("scope:a", false))
	h.run("scope:a")
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRedirect, Reason: "off track", Guidance: "restart on the parser",
	})
	if res.Results[0].Outcome != supervisionapi.OutcomeClaimed {
		t.Fatalf("redirect = %+v, want claimed", res.Results[0])
	}
	claimed := res.Results[0].Generation

	// The successor registers: the adoption happened, so the claim is nobody's
	// to void.
	h.run("scope:a")

	h.batch.QueueRuleStop("scope:a", "the rule says stop")
	res = h.next()
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v, want only the stop: the claim's generation moved on", res.Results)
	}
	if got := res.Results[0]; got.Action != supervisionapi.ActionStop || got.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[0] = %+v, want the stop applied", got)
	}
	if got := h.stops; len(got) != 1 || got[0] != "scope:a" {
		t.Errorf("stops delivered = %v, want one stop of scope:a (it targets whatever run is live)", got)
	}
	// The claim was pruned silently rather than recorded.
	if n := h.pendingClaims(); n != 0 {
		t.Errorf("pending claims = %d after the stop, want 0", n)
	}
	for _, r := range res.Results {
		if r.DropReason == supervisionapi.DropReasonSupersededByStop {
			t.Errorf("a false supersede was recorded: %+v", r)
		}
		if r.Generation == claimed {
			t.Errorf("a record carries the adopted claim's generation %d: %+v", claimed, r)
		}
	}
}

// TestBatch_StopExemptFromCap pins the stop's exemption in both directions: the
// cap never blocks it and it never spends the cap, so the budget a policy steer
// spent stays spent exactly once.
func TestBatch_StopExemptFromCap(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: 0, MaxInterventions: 1})
	h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
	h.run("scope:a")
	h.run("scope:b")
	h.render()

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "first"}); len(res.Applied) != 1 {
		t.Fatalf("the spending steer = %+v, want applied", res.Results)
	}
	if got := h.spent(); got != 1 {
		t.Fatalf("interventions = %d after one steer, want 1", got)
	}

	h.batch.QueueRuleStop("scope:a", "the safety valve")
	res := h.next()
	if len(res.Results) != 1 || res.Results[0].Outcome != supervisionapi.OutcomeApplied {
		t.Fatalf("a rules stop with the cap spent = %+v, want it delivered anyway", res.Results)
	}
	if got := h.stops; len(got) != 1 {
		t.Errorf("stops delivered = %v, want the stop", got)
	}
	if got := h.spent(); got != 1 {
		t.Errorf("interventions = %d after a stop, want 1: a stop neither blocks on nor spends the cap", got)
	}

	res = h.next(supervisionapi.Decision{Scope: "w2", Action: supervisionapi.ActionSteer, Guidance: "second"})
	if res.Results[0].DropReason != supervisionapi.DropReasonCapReached {
		t.Errorf("the next policy intervention = %+v, want cap_reached", res.Results[0])
	}
}

// TestBatch_RootScopeDecisionDropped pins the root as a non-target: a decision
// naming it by raw scope ID is dropped with root_scope and reaches nothing.
func TestBatch_RootScopeDecisionDropped(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{RootScopeID: "scope:root"})
	h.src.set(agentSnap("scope:root", true), agentSnap("scope:a", true))
	root := h.run("scope:root")
	run := h.run("scope:a")
	h.render()

	res := h.next(supervisionapi.Decision{
		Scope: "scope:root", Action: supervisionapi.ActionSteer, Guidance: "meddle with the parent",
	})
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v, want one record", res.Results)
	}
	if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeDropped ||
		got.DropReason != supervisionapi.DropReasonRootScope {
		t.Errorf("root-scope decision = %+v, want dropped/root_scope", got)
	}
	if len(res.Applied) != 0 {
		t.Errorf("applied = %+v, want none", res.Applied)
	}
	if got := drainGuidance(root.steer); len(got) != 0 {
		t.Errorf("the root run was steered: %q", got)
	}
	if got := drainGuidance(run.steer); len(got) != 0 {
		t.Errorf("a worker was steered: %q", got)
	}
}

// TestBatch_ShouldDecideNeedsALiveWorker pins design §4.4's liveness clause: the
// always trigger asks for a decider on every tick that has a LIVE agent, and a
// tick with no live worker is skipped — the caller waits on Done instead.
func TestBatch_ShouldDecideNeedsALiveWorker(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{Trigger: TriggerAlways})
	// One live worker: the decider is wanted, even before the first tick —
	// ShouldDecide takes the bootstrap snapshot.
	h.src.set(agentSnap("scope:a", true))
	if !h.batch.ShouldDecide() {
		t.Error("the always trigger withheld the decider with a live worker")
	}

	h.src.set(agentSnap("scope:a", false))
	if res := h.next(); !res.Done {
		t.Error("Done after the last worker finished = false, want true")
	}
	if h.batch.ShouldDecide() {
		t.Error("the always trigger asked for a decider with no live worker")
	}

	t.Run("the root is not a worker", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{Trigger: TriggerAlways, RootScopeID: "scope:root"})
		h.src.set(agentSnap("scope:root", true))
		if h.batch.ShouldDecide() {
			t.Error("a lone live root asked for a decider: the root is never a batch member")
		}
	})
}

// TestBatch_CancelledTickAgesInterventions pins the clock the cancelled tick
// still advances: the tick number moves on (endTick), so the intervention ages
// move with it. A cancelled tick that froze the ages would stretch the cooldown
// by a tick and render ticks_ago=0 for an intervention that happened.
func TestBatch_CancelledTickAgesInterventions(t *testing.T) {
	calls := 0
	h := newBatchHarnessWithWait(t, BatchConfig{CooldownTicks: -1}, func(context.Context, time.Duration) error {
		calls++
		if calls == 2 {
			return context.Canceled
		}
		return nil
	})
	h.src.set(agentSnap("scope:a", true))
	run := h.run("scope:a")
	h.render()

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "first"}); len(res.Applied) != 1 {
		t.Fatalf("first tick applied = %+v, want one steer", res.Applied)
	}
	if text := h.render(); !strings.Contains(text, "ticks_ago=1") {
		t.Fatalf("the first intervention did not age to one tick:\n%s", text)
	}
	drainGuidance(run.steer)

	res, err := h.batch.Next(context.Background(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tick = %v, want context.Canceled", err)
	}
	if res.Done {
		t.Error("Done during the cancelled tick = true, want false (the worker is still live)")
	}
	if text := h.render(); !strings.Contains(text, "ticks_ago=2") {
		t.Errorf("the cancelled tick did not age the intervention (want ticks_ago=2):\n%s", text)
	}

	if res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "second"}); len(res.Applied) != 1 {
		t.Fatalf("steer after the cancelled tick = %+v, want applied: a cancelled tick must not extend the cooldown", res.Results)
	}
}

// recordingArbiter is a RuleArbiter that records what the Service queued.
type recordingArbiter struct {
	mu     sync.Mutex
	steers []string
	stops  []string
}

func (r *recordingArbiter) QueueRuleSteer(scopeID, reason, guidance string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steers = append(r.steers, scopeID+"|"+reason+"|"+guidance)
}

func (r *recordingArbiter) QueueRuleStop(scopeID, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops = append(r.stops, scopeID+"|"+reason)
}

func (r *recordingArbiter) steerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.steers)
}

func (r *recordingArbiter) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stops)
}

// steerAt and stopAt return one queued entry as "scope|reason|guidance" (steer)
// or "scope|reason" (stop), so a test can pin the payload and not just the
// count.
func (r *recordingArbiter) steerAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.steers) {
		return ""
	}
	return r.steers[i]
}

func (r *recordingArbiter) stopAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.stops) {
		return ""
	}
	return r.stops[i]
}

// hasInjectFact reports whether the scope's trace holds a supervisor:inject
// fact — the record deliverInject writes on the direct path.
func hasInjectFact(t *testing.T, store *kerneltrace.SQLiteTraceStore, owner string) bool {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(context.Background(), kerneltrace.TrustedReadContext, owner, 999, kerneltrace.ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	for _, id := range slice.FactIDs() {
		if slice.FactsByID[id].GetEnvelope().SchemaRef == goatherd.SchemaSupervisorInject {
			return true
		}
	}
	return false
}

// TestService_RoutesRuleFiringsThroughArbiter pins the optional Service hook
// (design §4.6, "the rules engine keeps what it fires, but routes through the
// same arbiter"): with an arbiter set, an inject firing is queued instead of
// delivered, and a halt firing is queued instead of halted.
func TestService_RoutesRuleFiringsThroughArbiter(t *testing.T) {
	t.Run("inject is queued, not delivered", func(t *testing.T) {
		svc, store, steer, bus := newTestService(t, allAdvisoryRules())
		arb := &recordingArbiter{}
		svc.SetArbiter(arb)

		publish(bus, kerneltrace.EffectEvent{
			TraceOwnerID: "sub-worker",
			Mode:         kerneltrace.Declaration,
			SchemaRef:    "yaah.tool.bash.v1",
			KindLabel:    "bash",
			Payload:      map[string]any{"cmd": "rm -rf /tmp/probe"},
		})

		deadline := time.Now().Add(2 * time.Second)
		for arb.steerCount() == 0 {
			if !time.Now().Before(deadline) {
				t.Fatal("the rule firing never reached the arbiter")
			}
			time.Sleep(time.Millisecond)
		}
		// The queued entry carries the payload the batch will actually deliver:
		// the scope, the engine-level reason (the kernel intervention names no
		// rule, so Firings keeps the rule name) and the rule's guidance verbatim.
		if got, want := arb.steerAt(0), "scope:sub-worker|"+ruleSteerReason+"|"; !strings.HasPrefix(got, want) {
			t.Errorf("queued steer = %q, want prefix %q", got, want)
		}
		if got := arb.steerAt(0); !strings.Contains(got, "destructive") {
			t.Errorf("queued steer = %q, want the rule's own guidance forwarded", got)
		}
		if got := drainGuidance(steer); len(got) != 0 {
			t.Errorf("the firing was also delivered directly: %q", got)
		}
		// The trace fact belongs to the direct path; the queued path's record
		// is the Batch's InterventionRecord instead.
		if hasInjectFact(t, store, "sub-worker") {
			t.Error("the queued path also wrote the supervisor:inject fact")
		}
	})

	t.Run("halt is queued, not halted", func(t *testing.T) {
		svc, _, _, bus := newTestService(t, AdvisoryRules{})
		arb := &recordingArbiter{}
		svc.SetArbiter(arb)
		svc.sup.AddRule(haltRule("explode"))

		publish(bus, kerneltrace.EffectEvent{
			TraceOwnerID: "sub-worker",
			Mode:         kerneltrace.Declaration,
			SchemaRef:    "yaah.tool.bash.v1",
			KindLabel:    "explode",
		})

		deadline := time.Now().Add(2 * time.Second)
		for arb.stopCount() == 0 {
			if !time.Now().Before(deadline) {
				t.Fatal("the halt firing never reached the arbiter")
			}
			time.Sleep(time.Millisecond)
		}
		if scope, ok := svc.mgr.Get("scope:sub-worker"); ok && scope.State() == goatherd.ScopeDiscarded {
			t.Error("the scope was halted: the queued stop must not reach deliverHalt")
		}
		if got, want := arb.stopAt(0), "scope:sub-worker|test halt"; got != want {
			t.Errorf("queued stop = %q, want %q", got, want)
		}
	})

	t.Run("a root halt never reaches the queue without the opt-in", func(t *testing.T) {
		// supervisor.halt_root is off in newTestService. The gate has to hold at
		// QUEUE time too: a nil or thin StopFunc would otherwise make the root
		// stoppable through the batch.
		svc, _, _, bus := newTestService(t, AdvisoryRules{})
		arb := &recordingArbiter{}
		svc.SetArbiter(arb)
		svc.sup.AddRule(haltRule("explode"))

		publishRootExplode(bus)

		// Give the drain a beat to (wrongly) queue, then assert it did not.
		time.Sleep(100 * time.Millisecond)
		if n := arb.stopCount(); n != 0 {
			t.Errorf("queued stops = %d (%v), want none: supervisor.halt_root is off", n, arb.stopAt(0))
		}
	})

	t.Run("nil arbiter keeps direct delivery", func(t *testing.T) {
		// Unchanged behavior: the existing service tests (for example
		// TestService_HaltDispatchSubScope) pin it without an arbiter set.
		svc, _, _, _ := newTestService(t, AdvisoryRules{})
		if svc.ruleArbiter() != nil {
			t.Error("a freshly built service has an arbiter set")
		}
	})
}

// TestService_StopFuncReportsRefusals pins the batch's stop adapter: every
// refusal the halt path makes on purpose is reported as delivered=false (so the
// arbiter records stop_refused instead of a stop that never happened), and a
// halt that lands is reported as delivered.
func TestService_StopFuncReportsRefusals(t *testing.T) {
	t.Run("root without the opt-in", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, AdvisoryRules{})
		// A cancellable root run, so a false verdict cannot be blamed on the
		// ErrNotHaltable refusal further down the path.
		var cancelled atomic.Bool
		svc.runs.Register("scope:sess-root", Run{
			SteerCh: make(chan string, SteerBuffer),
			Cancel:  func() { cancelled.Store(true) },
		})

		delivered, err := svc.StopFunc()(context.Background(), "scope:sess-root", "the rule wants the root stopped")
		if err != nil {
			t.Fatalf("StopFunc on the root = %v, want a refusal, not a failure", err)
		}
		if delivered {
			t.Error("the root was reported stopped with supervisor.halt_root off")
		}
		if cancelled.Load() {
			t.Error("the root run was cancelled: the gate refused nothing")
		}
	})

	t.Run("run with no cancellation", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, AdvisoryRules{})
		// newTestService registers scope:sub-worker with a steering channel and
		// no cancellation: the kernel halt would tear the sandbox down under a
		// live run, so the halt path refuses.
		delivered, err := svc.StopFunc()(context.Background(), "scope:sub-worker", "the rule wants it stopped")
		if err != nil {
			t.Fatalf("StopFunc on an unstoppable run = %v, want a refusal, not a failure", err)
		}
		if delivered {
			t.Error("a run that registered no cancellation was reported stopped")
		}
		if scope, ok := svc.mgr.Get("scope:sub-worker"); ok && scope.State() == goatherd.ScopeDiscarded {
			t.Error("the scope was halted anyway")
		}
	})

	t.Run("delivered", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, AdvisoryRules{})
		quiesce := make(chan struct{})
		svc.runs.Register("scope:sub-worker", Run{
			SteerCh: make(chan string, SteerBuffer),
			Cancel:  func() { close(quiesce) },
			Done:    quiesce,
		})

		delivered, err := svc.StopFunc()(context.Background(), "scope:sub-worker", "the rule wants it stopped")
		if err != nil {
			t.Fatalf("StopFunc = %v, want nil", err)
		}
		if !delivered {
			t.Fatal("a delivered stop reported delivered=false")
		}
		if scope, ok := svc.mgr.Get("scope:sub-worker"); !ok || scope.State() != goatherd.ScopeDiscarded {
			t.Errorf("scope state = %q (found %v), want discarded", scope.State(), ok)
		}
	})
}

// TestBatch_RootScopeIsNotRedirectable pins the ENFORCED root boundary: the
// registration's Redirectable flag, not BatchConfig.RootScopeID, is what keeps a
// redirect and a revert off the session's own loop. With no RootScopeID
// configured the root is an ordinary member — which is why the flag has to carry
// the protection — and a steer on it is a separate question, governed by the
// RootScopeID guard alone.
func TestBatch_RootScopeIsNotRedirectable(t *testing.T) {
	setup := func(t *testing.T) (*batchHarness, *fakeRun) {
		t.Helper()
		h := newBatchHarness(t, BatchConfig{}) // no RootScopeID: the root is a member
		h.src.set(agentSnap("scope:root", true))
		root := h.runRootLike("scope:root")
		h.render()
		return h, root
	}

	t.Run("redirect is dropped", func(t *testing.T) {
		h, root := setup(t)
		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "restart it",
		})
		if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeDropped ||
			got.DropReason != supervisionapi.DropReasonNotRedirectable {
			t.Fatalf("redirect on a root-like registration = %+v, want dropped/not_redirectable", got)
		}
		if len(res.Applied) != 0 || h.pendingClaims() != 0 {
			t.Errorf("applied = %+v, claims = %d, want nothing claimed", res.Applied, h.pendingClaims())
		}
		if root.stopped != 0 {
			t.Errorf("the root run was stopped (%d) by a dropped redirect", root.stopped)
		}
		// Proof by probe: the generation was never claimed.
		gen, ok := h.runs.Generation("scope:root")
		if !ok {
			t.Fatal("the root run is gone: a dropped redirect must leave it running")
		}
		if _, err := h.runs.Redirect("scope:root", gen, supervisionapi.RedirectRequest{Guidance: "probe"}); err != nil {
			t.Errorf("the generation is already claimed (%v): the dropped redirect reached Redirect", err)
		}
	})

	t.Run("revert is dropped as not_redirectable", func(t *testing.T) {
		h, _ := setup(t)
		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRevert, Guidance: "undo and restart",
		})
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonNotRedirectable {
			t.Errorf("revert on a root-like registration = %+v, want not_redirectable (the boundary is asked first)", got)
		}
		if h.pendingClaims() != 0 {
			t.Errorf("claims = %d, want none", h.pendingClaims())
		}
	})

	t.Run("steer with the root scope ID set is dropped", func(t *testing.T) {
		h := newBatchHarness(t, BatchConfig{RootScopeID: "scope:root"})
		h.src.set(agentSnap("scope:root", true))
		root := h.runRootLike("scope:root")
		h.render()

		res := h.next(supervisionapi.Decision{
			Scope: "scope:root", Action: supervisionapi.ActionSteer, Guidance: "meddle with the parent",
		})
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonRootScope {
			t.Errorf("steer on the root with RootScopeID set = %+v, want root_scope", got)
		}
		if got := drainGuidance(root.steer); len(got) != 0 {
			t.Errorf("the root run was steered: %q", got)
		}
	})

	t.Run("steer with the root scope ID EMPTY is allowed by design", func(t *testing.T) {
		// Pinned deliberately: Redirectable protects the root from a handoff,
		// not from guidance. With no RootScopeID configured nothing names the
		// root, so the steer is an ordinary steer — set the field to close it.
		h, root := setup(t)
		res := h.next(supervisionapi.Decision{
			Scope: "scope:root", Action: supervisionapi.ActionSteer, Guidance: "nudge the parent",
		})
		if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeApplied {
			t.Fatalf("steer on a root with no RootScopeID configured = %+v, want applied (the documented gap)", got)
		}
		if got := drainGuidance(root.steer); len(got) != 1 || got[0] != "nudge the parent" {
			t.Errorf("delivered = %q, want the steer: RootScopeID is what guards a root steer", got)
		}
	})
}

// TestBatch_SameTickRulesSteerDoesNotCoolOutPolicy pins the cooldown ordering
// between the tick's two halves: the policy half reads the cooldown set as it
// stood BEFORE the rules pass, so a rules steer applied in this tick cannot cool
// out a higher-precedence policy revert on the same scope in the same tick. It
// still cools NEXT tick, because ages bump at tick end.
func TestBatch_SameTickRulesSteerDoesNotCoolOutPolicy(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: -1}) // the default: one tick
	h.src.set(handoffSnap("scope:a", true))
	run := h.run("scope:a")
	h.capture("scope:a")
	h.render()

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	res := h.next(supervisionapi.Decision{
		Scope: "w1", Action: supervisionapi.ActionRevert, Reason: "it broke the tree", Guidance: "undo and retry",
	})
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the rules steer then the policy revert", res.Results)
	}
	if got := res.Results[0]; got.Source != supervisionapi.SourceRules || got.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[0] = %+v, want the rules steer applied", got)
	}
	if got := res.Results[1]; got.Outcome != supervisionapi.OutcomeClaimed || got.Action != supervisionapi.ActionRevert {
		t.Errorf("results[1] = %+v, want the revert claimed, not cooled out by this tick's own rules steer", got)
	}
	if len(res.Applied) != 2 {
		t.Errorf("applied = %+v, want the steer and the claim", res.Applied)
	}
	if run.stopped != 1 {
		t.Errorf("handoff stops = %d, want 1 (the claimed revert's own stop)", run.stopped)
	}

	// The rule's action does count from the NEXT tick on: the age bumped at tick
	// end, so a steer on the same scope is inside the window now.
	res = h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "go again"})
	if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonCooldown {
		t.Errorf("next-tick steer = %+v, want cooldown (this tick's action ages at tick end)", got)
	}
}

// TestBatch_ClaimRevalidatesAgainstTheLiveRegistration pins the claim-time
// incarnation check: the gauntlet's redirectable/reversible facts come from the
// last ingested snapshot, so they are read again from the LIVE registration
// before the claim — a successor whose capture never landed must not inherit the
// predecessor's Reversible, and a run that vanished must not be claimed.
func TestBatch_ClaimRevalidatesAgainstTheLiveRegistration(t *testing.T) {
	setup := func(t *testing.T) *batchHarness {
		t.Helper()
		h := newBatchHarness(t, BatchConfig{})
		h.src.set(handoffSnap("scope:a", true)) // the snapshot says: claimable, capture exists
		h.run("scope:a")
		h.capture("scope:a")
		h.render()
		return h
	}
	// assertUnclaimed proves the claim never reached Redirect: the live
	// generation is still free for another claimer.
	assertUnclaimed := func(t *testing.T, h *batchHarness) {
		t.Helper()
		if n := h.pendingClaims(); n != 0 {
			t.Errorf("pending claims = %d, want 0", n)
		}
		gen, ok := h.runs.Generation("scope:a")
		if !ok {
			return // no live run: nothing could be claimed anyway
		}
		if _, err := h.runs.Redirect("scope:a", gen, supervisionapi.RedirectRequest{Guidance: "probe"}); err != nil {
			t.Errorf("the live generation is already claimed (%v): the dropped decision reached Redirect", err)
		}
	}

	t.Run("a successor without the capture", func(t *testing.T) {
		h := setup(t)
		// The incarnation is replaced between the ingest and the claim, and the
		// successor's pre-run capture never landed.
		h.runs.Unregister("scope:a")
		h.run("scope:a")

		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRevert, Reason: "it broke the tree", Guidance: "undo and retry",
		})
		if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeDropped ||
			got.DropReason != supervisionapi.DropReasonNotReversible {
			t.Fatalf("revert against an unmarked successor = %+v, want dropped/not_reversible", got)
		}
		assertUnclaimed(t, h)
	})

	t.Run("a successor that cannot be redirected", func(t *testing.T) {
		h := setup(t)
		h.runs.Unregister("scope:a")
		h.runRootLike("scope:a") // registers no handoff

		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over",
		})
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonNotRedirectable {
			t.Fatalf("redirect against a non-redirectable successor = %+v, want not_redirectable", got)
		}
		assertUnclaimed(t, h)
	})

	t.Run("a run that ended", func(t *testing.T) {
		h := setup(t)
		h.runs.Unregister("scope:a")

		res := h.next(supervisionapi.Decision{
			Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over",
		})
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonNoRun {
			t.Fatalf("redirect against a vanished run = %+v, want no_run", got)
		}
		if n := h.pendingClaims(); n != 0 {
			t.Errorf("pending claims = %d, want 0", n)
		}
	})
}

// TestBatch_RuleQueueIsBounded pins maxQueuedRules: a composition that sets the
// arbiter and then stops ticking cannot accumulate firings for the session's
// life. The NEWEST firing is dropped, the drops are counted, and a tick still
// drains what the queue holds.
func TestBatch_RuleQueueIsBounded(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	extra := 5
	for i := 0; i < maxQueuedRules+extra; i++ {
		h.batch.QueueRuleSteer("scope:a", "stuck", "try something else")
	}
	h.batch.mu.Lock()
	queued, dropped := len(h.batch.rules), h.batch.rulesDropped
	h.batch.mu.Unlock()
	if queued != maxQueuedRules {
		t.Fatalf("queued rules = %d, want the cap %d", queued, maxQueuedRules)
	}
	if dropped != extra {
		t.Errorf("dropped = %d, want %d", dropped, extra)
	}
	if got := h.batch.RulesDropped(); got != extra {
		t.Errorf("RulesDropped() = %d, want %d", got, extra)
	}

	// A stop queued into the full queue is dropped the same way (the newest
	// loses), and it does not displace a queued steer.
	h.batch.QueueRuleStop("scope:a", "the rule says stop")
	if got := h.batch.RulesDropped(); got != extra+1 {
		t.Errorf("RulesDropped() after a stop into a full queue = %d, want %d", got, extra+1)
	}

	h.src.set(agentSnap("scope:a", true))
	h.run("scope:a")
	h.render()
	h.next()
	h.batch.mu.Lock()
	after := len(h.batch.rules)
	h.batch.mu.Unlock()
	if after != 0 {
		t.Errorf("queued rules after a tick = %d, want 0 (the queue drains)", after)
	}
}

// TestBatch_AppliedRulesSteerSuppressesPolicySteer pins the OTHER half of the
// rules-vs-policy steer boundary: a rules steer that actually lands this tick
// suppresses a policy steer on the same scope (the cooldown-dropped variant is
// TestBatch_RulesSteerInsideCooldownSuppressesPolicy), and it spends the budget
// as an ordinary intervention.
func TestBatch_AppliedRulesSteerSuppressesPolicySteer(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{})
	h.src.set(agentSnap("scope:a", true))
	run := h.run("scope:a")
	h.render()

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	res := h.next(supervisionapi.Decision{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "policy says go"})
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the rules steer then the policy steer", res.Results)
	}
	if got := res.Results[0]; got.Source != supervisionapi.SourceRules || got.Outcome != supervisionapi.OutcomeApplied {
		t.Errorf("results[0] = %+v, want the rules steer applied", got)
	}
	if got := res.Results[1]; got.Source != supervisionapi.SourcePolicy ||
		got.DropReason != supervisionapi.DropReasonSuperseded {
		t.Errorf("results[1] = %+v, want the policy steer dropped as superseded", got)
	}
	if len(res.Applied) != 1 {
		t.Errorf("applied = %+v, want only the rules steer", res.Applied)
	}
	if got := drainGuidance(run.steer); len(got) != 1 || got[0] != "try a different approach" {
		t.Errorf("delivered = %q, want the rules steer only", got)
	}
	if got := h.spent(); got != 1 {
		t.Errorf("interventions = %d, want 1: a rules steer spends the cap like a policy steer", got)
	}
}

// TestBatch_RulesSteerSpendsTheCap pins the accounting order across the two
// halves: a rules steer spends the LAST unit of the budget, so the policy action
// the same tick reports cap_reached.
func TestBatch_RulesSteerSpendsTheCap(t *testing.T) {
	h := newBatchHarness(t, BatchConfig{CooldownTicks: 0, MaxInterventions: 1})
	h.src.set(agentSnap("scope:a", true), agentSnap("scope:b", true))
	h.run("scope:a")
	h.run("scope:b")
	h.render()

	h.batch.QueueRuleSteer("scope:a", "stuck", "try a different approach")
	res := h.next(supervisionapi.Decision{Scope: "w2", Action: supervisionapi.ActionSteer, Guidance: "policy says go"})
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v, want the rules steer then the policy steer", res.Results)
	}
	if got := res.Results[0]; got.Outcome != supervisionapi.OutcomeApplied {
		t.Fatalf("results[0] = %+v, want the rules steer applied (it has the last unit)", got)
	}
	if got := res.Results[1]; got.DropReason != supervisionapi.DropReasonCapReached {
		t.Errorf("results[1] = %+v, want cap_reached: the rules steer already spent the budget", got)
	}
	if got := h.spent(); got != 1 {
		t.Errorf("interventions = %d, want 1", got)
	}
}

// TestBatch_NilRegistryAndNilSource pins the documented degenerate
// constructions. NewBatch(nil, nil, ...) yields the empty snapshot, so the batch
// is Done at once — and a decision cannot even resolve a target, because the
// empty snapshot knows no scope: the drop is stale, not no_run. The no_run
// fallback is the NIL REGISTRY's ("nil makes every claim drop no_run"), which
// needs a source that names a live scope to reach.
func TestBatch_NilRegistryAndNilSource(t *testing.T) {
	ctx := context.Background()

	t.Run("nil source", func(t *testing.T) {
		b := NewBatch(nil, nil, BatchConfig{Now: testClock})
		res, err := b.Next(ctx, []supervisionapi.Decision{
			{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "hello"},
		})
		if err != nil {
			t.Fatalf("Next = %v, want nil", err)
		}
		if !res.Done {
			t.Error("Done with a nil snapshot source = false, want true (nothing is running that it can see)")
		}
		if len(res.Snapshot.Agents) != 0 {
			t.Errorf("snapshot agents = %+v, want none", res.Snapshot.Agents)
		}
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonStale {
			t.Errorf("steer against the empty snapshot = %+v, want stale (no label and no scope resolves)", got)
		}
		if len(res.Applied) != 0 {
			t.Errorf("applied = %+v, want none", res.Applied)
		}
	})

	t.Run("nil registry", func(t *testing.T) {
		src := &fakeSource{}
		src.set(handoffSnap("scope:a", true))
		b := NewBatch(nil, src, BatchConfig{
			CooldownTicks: 0,
			Now:           testClock,
			Wait:          func(context.Context, time.Duration) error { return nil },
		})
		if text := b.SnapshotText(0); !strings.Contains(text, "w1") {
			t.Fatalf("the render names no worker:\n%s", text)
		}

		res, err := b.Next(ctx, []supervisionapi.Decision{
			{Scope: "w1", Action: supervisionapi.ActionSteer, Guidance: "hello"},
		})
		if err != nil {
			t.Fatalf("Next = %v, want nil", err)
		}
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonNoRun {
			t.Errorf("steer with no registry = %+v, want no_run", got)
		}

		res, err = b.Next(ctx, []supervisionapi.Decision{
			{Scope: "w1", Action: supervisionapi.ActionRedirect, Guidance: "start over"},
		})
		if err != nil {
			t.Fatalf("Next = %v, want nil", err)
		}
		if got := res.Results[0]; got.DropReason != supervisionapi.DropReasonNoRun {
			t.Errorf("redirect with no registry = %+v, want no_run", got)
		}
		if n := func() int {
			b.mu.Lock()
			defer b.mu.Unlock()
			return len(b.claims)
		}(); n != 0 {
			t.Errorf("claims = %d, want none", n)
		}
	})
}
