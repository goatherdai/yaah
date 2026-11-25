package supervision

// headless_test.go pins the headless (measurement) decider and its loop
// (yaah-0nbb, design §4.8): the fail-closed parse, the one-shot-per-tick
// cadence, the on-change trigger skipping the model call, and the loop ending
// at Done rather than outliving its batch.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	renderer "github.com/goatherdai/yaah/internal/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// scriptedSource is a SnapshotSource whose answer is a function of the call
// number, so a test can say "a worker is live for the first two polls, then it
// is gone" — which is what makes a batch report Done.
type scriptedSource struct {
	calls  atomic.Int64
	agents func(call int64) []supervisionapi.AgentSnapshot
}

func (s *scriptedSource) Snapshot() supervisionapi.Snapshot {
	n := s.calls.Add(1)
	return supervisionapi.Snapshot{At: time.Unix(0, 0), Agents: s.agents(n)}
}

// liveAgent is one running worker with the fields the renderer reads.
func liveAgent(scope string) supervisionapi.AgentSnapshot {
	return supervisionapi.AgentSnapshot{
		ScopeID:      scope,
		OwnerID:      strings.TrimPrefix(scope, "scope:"),
		Role:         "golang-developer",
		Feature:      "fix the parser",
		StillRunning: true,
		Redirectable: true,
		Reversible:   true,
	}
}

// instantWait makes a tick cost no wall clock: the batch's own Wait seam.
func instantWait(_ context.Context, _ time.Duration) error { return nil }

// newLoopBatch builds a batch over src with the test cadence.
func newLoopBatch(src supervisionapi.SnapshotSource, trigger Trigger) *Batch {
	return NewBatch(nil, src, BatchConfig{
		Interval: time.Millisecond,
		Wait:     instantWait,
		Trigger:  trigger,
	})
}

const noDecisionsReply = `{"decisions": []}`

// TestParseDecisions_FailClosed pins the parse contract: the reply the locked
// prompt's footer asks for parses, and every way it can be unreadable drops the
// WHOLE reply rather than the part that happened to decode.
func TestParseDecisions_FailClosed(t *testing.T) {
	labels := []renderer.Labeled{{Label: "w1", ScopeID: "scope:sub-a"}, {Label: "w2", ScopeID: "scope:sub-b"}}
	cases := []struct {
		name    string
		reply   string
		want    int
		wantErr string
	}{
		{
			name:  "a conforming list",
			reply: `{"decisions": [{"scope": "w1", "action": "steer", "reason": "drifting", "guidance": "stay on the parser"}]}`,
			want:  1,
		},
		{
			name:  "a fenced reply still parses",
			reply: "Here you go:\n```json\n{\"decisions\": [{\"scope\": \"w2\", \"action\": \"redirect\", \"guidance\": \"test first\"}]}\n```",
			want:  1,
		},
		{
			name:  "an explicit none is dropped, not recorded",
			reply: `{"decisions": [{"scope": "w1", "action": "none"}]}`,
			want:  0,
		},
		{name: "no JSON at all", reply: "I think w1 is fine.", wantErr: "not a JSON object"},
		{name: "no decisions key", reply: `{"result": []}`, wantErr: `no "decisions" key`},
		{name: "decisions is not a list", reply: `{"decisions": "none"}`, wantErr: "not a list"},
		{name: "an entry is not an object", reply: `{"decisions": ["steer w1"]}`, wantErr: "not an object"},
		{name: "a non-string action", reply: `{"decisions": [{"scope": "w1", "action": 3}]}`, wantErr: "is not a string"},
		{name: "an unknown action", reply: `{"decisions": [{"scope": "w1", "action": "handoff"}]}`, wantErr: "not one of"},
		{
			// The rules engine's stop is not a decider verb (design D6).
			name:    "the rules engine's stop is refused",
			reply:   `{"decisions": [{"scope": "w1", "action": "stop"}]}`,
			wantErr: "not one of",
		},
		{name: "an unassigned scope", reply: `{"decisions": [{"scope": "w9", "action": "steer"}]}`, wantErr: "did not assign"},
		{
			name:    "one scope named twice",
			reply:   `{"decisions": [{"scope": "w1", "action": "steer"}, {"scope": "w1", "action": "redirect"}]}`,
			wantErr: "a second time",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDecisions(tc.reply, labels)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseDecisions = %v, %v; want an error naming %q", got, err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("a refused reply must carry NO decisions, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDecisions: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("decisions = %v, want %d", got, tc.want)
			}
		})
	}
}

// TestParseDecisions_TruncatesWordLimits pins the two bounds the renderer's
// footer advertises: over-long text is cut whole-word rather than refused,
// because the arbiter truncates identically and a refusal would throw away a
// usable intervention over its prose.
func TestParseDecisions_TruncatesWordLimits(t *testing.T) {
	labels := []renderer.Labeled{{Label: "w1", ScopeID: "scope:sub-a"}}
	reason := strings.TrimSpace(strings.Repeat("word ", supervisionapi.MaxReasonWords+10))
	guidance := strings.TrimSpace(strings.Repeat("step ", supervisionapi.MaxGuidanceWords+10))
	reply := `{"decisions": [{"scope": "w1", "action": "steer", "reason": "` + reason + `", "guidance": "` + guidance + `"}]}`
	got, err := parseDecisions(reply, labels)
	if err != nil {
		t.Fatalf("parseDecisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decisions = %v, want one", got)
	}
	if n := supervisionapi.WordCount(got[0].Reason); n != supervisionapi.MaxReasonWords {
		t.Errorf("reason words = %d, want the %d-word cap", n, supervisionapi.MaxReasonWords)
	}
	if n := supervisionapi.WordCount(got[0].Guidance); n != supervisionapi.MaxGuidanceWords {
		t.Errorf("guidance words = %d, want the %d-word cap", n, supervisionapi.MaxGuidanceWords)
	}
}

// TestHeadlessDecider_DecideAggregatesTriggers pins the interface entry point:
// Decide renders the snapshot itself and asks the model once, so a caller that
// only has supervisionapi.Decider (not the loop) still gets one call per
// snapshot — and no call at all for an empty batch.
func TestHeadlessDecider_DecideAggregatesTriggers(t *testing.T) {
	var calls atomic.Int64
	var sawSystem string
	d := &HeadlessDecider{
		Prompt: "LOCKED PROMPT",
		Complete: func(_ context.Context, system, snapshot string) (string, error) {
			calls.Add(1)
			sawSystem = system
			if !strings.Contains(snapshot, "w1") {
				t.Errorf("the rendered snapshot named no label: %q", snapshot)
			}
			return noDecisionsReply, nil
		},
	}
	if got, err := d.Decide(context.Background(), supervisionapi.Snapshot{}); err != nil || got != nil {
		t.Fatalf("empty snapshot: Decide = %v, %v; want no decisions and no error", got, err)
	}
	if calls.Load() != 0 {
		t.Error("an empty batch must not spend a model call")
	}
	if _, err := d.Decide(context.Background(), supervisionapi.Snapshot{
		Agents: []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")},
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("completion calls = %d, want 1", calls.Load())
	}
	if sawSystem != "LOCKED PROMPT" {
		t.Errorf("system prompt = %q, want the decider's locked prompt", sawSystem)
	}
}

// TestHeadlessDecider_RequiresPromptAndCompletion pins the two refusals that
// keep a misconfigured mode from looking like a quiet one.
func TestHeadlessDecider_RequiresPromptAndCompletion(t *testing.T) {
	noop := func(context.Context, string, string) (string, error) { return noDecisionsReply, nil }
	if _, err := (&HeadlessDecider{Complete: noop}).decide(context.Background(), "text", nil); err == nil {
		t.Error("a decider with no prompt must refuse rather than send an unprotocoled snapshot")
	}
	if _, err := (&HeadlessDecider{Prompt: "p"}).decide(context.Background(), "text", nil); err == nil {
		t.Error("a decider with no completion must refuse")
	}
}

// TestRunHeadlessLoop_DrivesBatchUntilDone pins the loop's shape: it ticks,
// spends one decider call per tick the trigger wants, applies through Next, and
// RETURNS when the batch reports Done — it does not wait to be cancelled.
func TestRunHeadlessLoop_DrivesBatchUntilDone(t *testing.T) {
	// A worker lives for the first three polls, then is gone: the batch reports
	// Done on the tick that ingests the empty snapshot.
	src := &scriptedSource{agents: func(call int64) []supervisionapi.AgentSnapshot {
		if call > 3 {
			return nil
		}
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	batch := newLoopBatch(src, TriggerAlways)
	var decides atomic.Int64
	dec := &HeadlessDecider{
		Prompt: "p",
		Complete: func(context.Context, string, string) (string, error) {
			decides.Add(1)
			return noDecisionsReply, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- RunHeadlessLoop(context.Background(), batch, dec, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHeadlessLoop = %v, want nil at Done", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not return when the batch reported Done")
	}
	if decides.Load() == 0 {
		t.Error("the decider was never asked: the loop ticked without deciding")
	}
	if tick := batch.Tick(); tick < 2 {
		t.Errorf("batch tick = %d, want at least 2 ticks before Done", tick)
	}
}

// TestRunHeadlessLoop_KeepsTickingAfterAGarbledReply pins the fail-closed
// contract at the loop level: a reply nobody can read costs ONE tick (no
// decisions applied) and one logged line — it does not end supervision.
func TestRunHeadlessLoop_KeepsTickingAfterAGarbledReply(t *testing.T) {
	src := &scriptedSource{agents: func(call int64) []supervisionapi.AgentSnapshot {
		if call > 4 {
			return nil
		}
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	batch := newLoopBatch(src, TriggerAlways)
	var replies atomic.Int64
	dec := &HeadlessDecider{
		Prompt: "p",
		Complete: func(context.Context, string, string) (string, error) {
			replies.Add(1)
			return "I could not decide anything, sorry", nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- RunHeadlessLoop(context.Background(), batch, dec, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHeadlessLoop = %v, want nil (a garbled reply must not kill the loop)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not return when the batch reported Done")
	}
	if replies.Load() < 2 {
		t.Errorf("decider calls = %d, want more than one: the loop must continue past a malformed reply", replies.Load())
	}
}

// TestRunHeadlessLoop_OnChangeSkipsIdenticalSnapshots pins the trigger's
// meaning for the caller: under on-change the model is asked once for an
// unchanged batch, while the loop keeps ticking (the batch still advances its
// clock and drains rule firings).
func TestRunHeadlessLoop_OnChangeSkipsIdenticalSnapshots(t *testing.T) {
	src := &scriptedSource{agents: func(call int64) []supervisionapi.AgentSnapshot {
		if call > 4 {
			return nil
		}
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	batch := newLoopBatch(src, TriggerOnChange)
	var replies atomic.Int64
	dec := &HeadlessDecider{
		Prompt: "p",
		Complete: func(context.Context, string, string) (string, error) {
			replies.Add(1)
			return noDecisionsReply, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- RunHeadlessLoop(context.Background(), batch, dec, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHeadlessLoop = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not return when the batch reported Done")
	}
	if got := replies.Load(); got != 1 {
		t.Errorf("decider calls = %d, want exactly 1 under on-change with an unchanged batch (ticks=%d)", got, batch.Tick())
	}
	if tick := batch.Tick(); tick < 3 {
		t.Errorf("batch tick = %d, want the loop to keep ticking past a skipped decide", tick)
	}
}

// TestRunHeadlessLoop_CancelledContextStops pins the teardown path: cancelling
// the loop's context ends it promptly with that error, which is how the host
// stops the loop when the dispatch set it was supervising is gone.
func TestRunHeadlessLoop_CancelledContextStops(t *testing.T) {
	src := &scriptedSource{agents: func(int64) []supervisionapi.AgentSnapshot {
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	batch := newLoopBatch(src, TriggerAlways)
	dec := &HeadlessDecider{
		Prompt:   "p",
		Complete: func(context.Context, string, string) (string, error) { return noDecisionsReply, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunHeadlessLoop(ctx, batch, dec, nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunHeadlessLoop = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the loop ignored its context cancellation")
	}
}

// TestHeadlessHost_StartsOneLoopArmsTheArbiterAndJoins pins the host's contract:
// one loop at a time, the rules arbiter armed for exactly the loop's lifetime,
// and the goroutine joined — not merely cancelled — by the time the loop is
// reported stopped.
func TestHeadlessHost_StartsOneLoopArmsTheArbiterAndJoins(t *testing.T) {
	src := &scriptedSource{agents: func(call int64) []supervisionapi.AgentSnapshot {
		if call > 3 {
			return nil
		}
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	var arms, releases atomic.Int64
	host := &HeadlessHost{
		NewBatch: func() *Batch { return newLoopBatch(src, TriggerAlways) },
		Decider: &HeadlessDecider{
			Prompt:   "p",
			Complete: func(context.Context, string, string) (string, error) { return noDecisionsReply, nil },
		},
		Arm:         func(RuleArbiter) { arms.Add(1) },
		Release:     func() { releases.Add(1) },
		JoinTimeout: 5 * time.Second,
	}
	if !host.Start(context.Background()) {
		t.Fatal("Start reported no loop on a fresh host")
	}
	if host.Start(context.Background()) {
		t.Error("a second Start must not launch a second loop")
	}
	deadline := time.Now().Add(5 * time.Second)
	for host.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if host.Running() {
		t.Fatal("the loop did not end on its own at Done")
	}
	if got := arms.Load(); got != 1 {
		t.Errorf("arbiter armed %d times, want exactly once", got)
	}
	if got := releases.Load(); got != 1 {
		t.Errorf("arbiter released %d times, want exactly once", got)
	}
	// A stopped host restarts cleanly: a new dispatch after the loop settled
	// must get a fresh loop (and a fresh arming), not a dead one.
	src.calls.Store(0)
	if !host.Start(context.Background()) {
		t.Fatal("Start after a settled loop must launch a fresh one")
	}
	if got := arms.Load(); got != 2 {
		t.Errorf("arbiter armed %d times after a restart, want 2", got)
	}
	host.Stop()
	if host.Running() {
		t.Error("Stop must leave no loop running")
	}
}

// TestHeadlessHost_SettleKeepsTheLoopWhileAWorkerLives pins the dispatch-side
// settle hook's real job: a dispatch settling does NOT stop supervision while
// another worker is still live, and the last one DOES stop and join it.
func TestHeadlessHost_SettleKeepsTheLoopWhileAWorkerLives(t *testing.T) {
	var live atomic.Bool
	live.Store(true)
	src := &scriptedSource{agents: func(int64) []supervisionapi.AgentSnapshot {
		if !live.Load() {
			return nil
		}
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	host := &HeadlessHost{
		NewBatch: func() *Batch { return newLoopBatch(src, TriggerAlways) },
		Decider: &HeadlessDecider{
			Prompt:   "p",
			Complete: func(context.Context, string, string) (string, error) { return noDecisionsReply, nil },
		},
		JoinTimeout: 5 * time.Second,
	}
	settle := host.RunHook(context.Background(), "scope:sub-a")
	if !host.Running() {
		t.Fatal("the run hook must start the loop")
	}
	// A worker is still live: this dispatch settling changes nothing.
	settle()
	if !host.Running() {
		t.Fatal("Settle stopped a loop that still had a live worker")
	}
	// The last worker finishes: the next settle stops AND joins it.
	live.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for host.Running() && time.Now().Before(deadline) {
		settle()
		time.Sleep(time.Millisecond)
	}
	if host.Running() {
		t.Fatal("Settle did not join the loop after the last worker finished")
	}
}

// TestHeadlessHost_StopJoinsACancelledLoop pins the teardown path: Stop returns
// only once the goroutine is gone, so a session that closes cannot leave a tick
// running against a registry it is tearing down.
func TestHeadlessHost_StopJoinsACancelledLoop(t *testing.T) {
	src := &scriptedSource{agents: func(int64) []supervisionapi.AgentSnapshot {
		return []supervisionapi.AgentSnapshot{liveAgent("scope:sub-a")}
	}}
	host := &HeadlessHost{
		NewBatch: func() *Batch { return newLoopBatch(src, TriggerAlways) },
		Decider: &HeadlessDecider{
			Prompt:   "p",
			Complete: func(context.Context, string, string) (string, error) { return noDecisionsReply, nil },
		},
		JoinTimeout: 5 * time.Second,
	}
	if !host.Start(context.Background()) {
		t.Fatal("Start reported no loop")
	}
	host.Stop()
	if host.Running() {
		t.Fatal("Stop left a loop running")
	}
	// Idempotent: a second Stop (session teardown after a settle) does nothing.
	host.Stop()
}
