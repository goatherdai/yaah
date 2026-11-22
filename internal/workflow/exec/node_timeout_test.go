package exec

// node_timeout_test.go pins the per-node timeout the SCHEDULER enforces
// (yaah-hxk3.5, the residue of Shepherd's `timeout` combinator). The bound is
// the node's own timeout_seconds, which the lane also honors in flight
// (Dispatch.Timeout); what is pinned here is the deterministic backstop, so
// every test drives the real driver cycle by cycle and moves the VIRTUAL clock
// between the boundaries the scheduler samples it at. Nothing sleeps, nothing
// arms a timer, and the clock is the only thing that decides a timeout.
//
// The acceptance criteria: a node past its declared bound fails with the same
// stop reason a lane-reported timeout uses, retries inside its retry ceiling,
// gets its work cancelled through the door, never fires when the door answered
// in time, and yields the same sequence every time it is run.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/trace"
)

// timeoutYAML is one delegate declaring a wall-clock bound, with a slot for a
// retry block. specHead supplies the document head (and its %s is the workflow
// name), so the format takes name, seconds, then the spliced block.
const timeoutYAML = specHead + `  steps:
    - id: slow
      kind: delegate
      role: developer
      prompt: do slow
      timeout_seconds: %d
%s`

// retryTwice is the retry block the retry test splices in: two attempts, no
// backoff, so the clock only ever moves for the deadlines and never for a wait.
const retryTwice = `      retry:
        max_attempts: 2
`

// newTimeoutRun builds an unstarted run over one timeout-declaring delegate and
// the fake that will drive it. The script is left empty on purpose: a test that
// wants a completion sets rt.Script before driving, and a test that wants the
// door to stall sets rt.Hold.
func newTimeoutRun(t *testing.T, seconds int, extra string) (*Run, *FakeRuntime) {
	t.Helper()
	g := graphFromYAML(t, fmt.Sprintf(timeoutYAML, "node-timeout", seconds, extra))
	rt := NewFakeRuntime(time.Unix(0, 0))
	r, err := New(g, rt, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, rt
}

// driveTimeoutRun advances the run one scheduler cycle at a time — never
// Execute, which hides the cycle boundaries a deadline is sampled at — calling
// tick before each cycle so a test can move the virtual clock. It fails the test
// if the run does not terminate, which is the "no hang" half of the contract.
func driveTimeoutRun(t *testing.T, r *Run, tick func(*Run)) Result {
	t.Helper()
	ctx := context.Background()
	for cycle := 0; !r.finished; cycle++ {
		if cycle >= 60 {
			t.Fatalf("run made %d cycles without terminating:\n%s", cycle, strings.Join(r.Events(), "\n"))
		}
		if tick != nil {
			tick(r)
		}
		progressed, err := r.Advance(ctx)
		if err != nil {
			t.Fatalf("Advance: %v\nlog:\n%s", err, strings.Join(r.Events(), "\n"))
		}
		if !progressed {
			t.Fatalf("Advance made no progress with the run unfinished:\n%s", strings.Join(r.Events(), "\n"))
		}
	}
	return r.Result()
}

// pastDeadline is the clock script: once an attempt is in flight, the virtual
// clock moves past any bound these tests declare. A fake charges no time for a
// HELD attempt, so without this the deadline would never be reachable — which is
// exactly the point: a timeout is the run clock crossing the bound, and nothing
// else.
func pastDeadline(rt *FakeRuntime, step time.Duration) func(*Run) {
	return func(r *Run) {
		if len(r.outstanding) > 0 {
			rt.Advance(step)
		}
	}
}

// dispatchOutcome finds the dispatch record a replay reads back for one dispatch
// id: the record that states the attempt's outcome, not the start marker that
// precedes it (internal/workflow/replay skips status=started for that reason).
func dispatchOutcome(recs []trace.Record, id string) (trace.Record, bool) {
	for _, rec := range recs {
		if rec.Kind == trace.KindDispatch && rec.DispatchID == id && rec.Status != trace.StatusStarted {
			return rec, true
		}
	}
	return trace.Record{}, false
}

// TestNodeTimeoutRejects pins the plain case: the door never answers, the run
// clock crosses the node's declared bound, and the node fails with the timeout
// stop reason instead of the run waiting forever for an attempt nobody will
// report on.
func TestNodeTimeoutRejects(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, "")
	rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
	rt.Hold = map[string]bool{"steps.slow": true}
	res := driveTimeoutRun(t, r, pastDeadline(rt, 6*time.Second))

	n := nodeOf(t, res, "steps.slow")
	if n.State != StateFailed {
		t.Fatalf("state = %s, want failed\nlog:\n%s", n.State, eventLog(res))
	}
	if n.StopReason != StopReasonTimeout {
		t.Errorf("stop reason = %q, want %q: a scheduler timeout states the lane's word", n.StopReason, StopReasonTimeout)
	}
	if !strings.Contains(n.Reason, "stop_reason=timeout") {
		t.Errorf("reason = %q, want it to name stop_reason=timeout", n.Reason)
	}
	if got := len(n.Attempts); got != 1 {
		t.Errorf("attempts = %d, want 1: an undeclared retry ceiling is one attempt", got)
	}
	// on_failure is undeclared, so the scheduler's folded default applies: the
	// failure aborts the run rather than exposing it and carrying on.
	if res.Status != RunAborted {
		t.Errorf("run status = %s, want aborted (reason %q)", res.Status, res.Reason)
	}
	log := eventLog(res)
	if !strings.Contains(log, string(EventAttemptTimedOut)) {
		t.Errorf("log has no %s event:\n%s", EventAttemptTimedOut, log)
	}
	// The line carries the evidence, as durations: the span the attempt ran and
	// the bound it breached. No timestamp ever enters the log.
	if want := "steps.slow#1 attempt=1 elapsed=6s timeout=5s"; !strings.Contains(log, want) {
		t.Errorf("log line does not state %q:\n%s", want, log)
	}
	if last := res.Events[len(res.Events)-1]; !strings.Contains(last, string(EventRunFinished)) {
		t.Errorf("last event = %q, want run_finished: a timeout must end the run, not strand it", last)
	}
}

// TestNodeTimeoutRetries pins the ceiling: a timeout is retryable, so the node
// is re-dispatched inside its declared retry.max_attempts, and the attempt that
// runs out the ceiling ends the node — exactly twice attempted, never more.
func TestNodeTimeoutRetries(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, retryTwice)
	rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
	rt.Hold = map[string]bool{"steps.slow": true}
	res := driveTimeoutRun(t, r, pastDeadline(rt, 6*time.Second))

	n := nodeOf(t, res, "steps.slow")
	if got := len(n.Attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (retry.max_attempts)\nlog:\n%s", got, eventLog(res))
	}
	for i, a := range n.Attempts {
		if a.Number != i+1 {
			t.Errorf("attempt %d number = %d, want %d", i, a.Number, i+1)
		}
		if a.StopReason != StopReasonTimeout {
			t.Errorf("attempt %d stop reason = %q, want %q", i+1, a.StopReason, StopReasonTimeout)
		}
	}
	if n.State != StateFailed {
		t.Errorf("state = %s, want failed after the ceiling was reached", n.State)
	}
	if !strings.Contains(n.Reason, "attempt ceiling 2 reached") {
		t.Errorf("reason = %q, want it to name the ceiling that closed the attempts", n.Reason)
	}
	if got := strings.Count(eventLog(res), string(EventAttemptTimedOut)); got != 2 {
		t.Errorf("timeout events = %d, want one per attempt", got)
	}
	if got, want := strings.Join(rt.Cancels(), ","), "steps.slow#1,steps.slow#2"; got != want {
		t.Errorf("door cancels = %s, want %s: every timed-out attempt is stopped", got, want)
	}
}

// TestNodeTimeoutUnderBudget pins the other side: a door that answers before the
// deadline is never timed out, and the run never asks the door to cancel work
// that finished. The bound is a backstop, not a race against the lane.
func TestNodeTimeoutUnderBudget(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, "")
	rt.Script = map[string][]Completion{"steps.slow": {Report("finished in time", 3*time.Second)}}
	// No clock hook: the only time that passes is the 3s the fake charges when
	// the completion is read back, which is inside the declared 5s.
	res := driveTimeoutRun(t, r, nil)

	if res.Status != RunSucceeded {
		t.Fatalf("status = %s, want succeeded (reason %q)\nlog:\n%s", res.Status, res.Reason, eventLog(res))
	}
	if got := len(rt.Cancels()); got != 0 {
		t.Errorf("door cancels = %d (%v), want none: the attempt finished before its deadline", got, rt.Cancels())
	}
	if strings.Contains(eventLog(res), string(EventAttemptTimedOut)) {
		t.Errorf("log states a timeout for an attempt that finished inside its bound:\n%s", eventLog(res))
	}
	n := nodeOf(t, res, "steps.slow")
	if n.State != StateSucceeded {
		t.Errorf("state = %s, want succeeded", n.State)
	}
	if got, want := res.Elapsed, 3*time.Second; got != want {
		t.Errorf("run elapsed = %s, want %s (the scripted attempt, below the bound)", got, want)
	}
}

// TestNodeTimeoutCancelsDoor pins that the scheduler does not merely declare the
// attempt dead: it asks the door to stop it, and the reducer records that the
// attempt was stopped. A door that keeps working work nobody will read is the
// failure mode this ordering exists to prevent.
func TestNodeTimeoutCancelsDoor(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, "")
	rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
	rt.Hold = map[string]bool{"steps.slow": true}
	res := driveTimeoutRun(t, r, pastDeadline(rt, 6*time.Second))

	cancels := rt.Cancels()
	if len(cancels) != 1 || cancels[0] != "steps.slow#1" {
		t.Fatalf("door cancels = %v, want exactly the timed-out dispatch steps.slow#1", cancels)
	}
	// The door really stopped it: the fake drops its hold and drops the attempt
	// from its in-flight count when Cancel lands.
	if got := rt.InFlight(); got != 0 {
		t.Errorf("door in flight = %d, want 0 after the cancel", got)
	}
	n := nodeOf(t, res, "steps.slow")
	if len(n.Attempts) != 1 || !n.Attempts[0].Cancelled {
		t.Errorf("recorded attempt = %+v, want one attempt marked cancelled", n.Attempts)
	}
}

// TestNodeTimeoutReplay pins that a timed-out run is reproducible: the same
// document, the same scripted door, and the same clock steps yield the same
// event sequence, the same door cancels, and a dispatch record a replay can
// answer from. That last part is what `internal/workflow/replay` reads — status,
// reason, and the retryable assert — so the timeout folds the same way a second
// time instead of arriving as an answer the record does not cover.
func TestNodeTimeoutReplay(t *testing.T) {
	drive := func() (Result, *FakeRuntime) {
		r, rt := newTimeoutRun(t, 5, retryTwice)
		rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
		rt.Hold = map[string]bool{"steps.slow": true}
		return driveTimeoutRun(t, r, pastDeadline(rt, 6*time.Second)), rt
	}
	res1, rt1 := drive()
	res2, rt2 := drive()

	if eventLog(res1) != eventLog(res2) {
		t.Errorf("event log differs between two runs of one timeout script:\n--- first\n%s\n--- second\n%s",
			eventLog(res1), eventLog(res2))
	}
	if got, want := strings.Join(rt1.Cancels(), ","), strings.Join(rt2.Cancels(), ","); got != want {
		t.Errorf("door cancels differ: %s vs %s", got, want)
	}
	if !strings.Contains(eventLog(res1), string(EventAttemptTimedOut)) {
		t.Fatalf("the scripted run did not time out, so there is nothing to replay:\n%s", eventLog(res1))
	}
	rec, ok := dispatchOutcome(res1.Records, "steps.slow#2")
	if !ok {
		t.Fatalf("no dispatch record for the replayed attempt steps.slow#2:\n%s", eventLog(res1))
	}
	if rec.Status != StatusError || rec.Reason != StopReasonTimeout {
		t.Errorf("record status=%q reason=%q, want status=error reason=timeout — the shape a replay re-folds",
			rec.Status, rec.Reason)
	}
	if rec.Retryable == nil || !*rec.Retryable {
		t.Errorf("record retryable = %v, want an asserted true: a retried timeout must replay as retryable", rec.Retryable)
	}
	if n := nodeOf(t, res1, "steps.slow"); n.Kind != ir.KindDelegate {
		t.Errorf("node kind = %s, want the delegate the record names", n.Kind)
	}
}

// TestNodeTimeoutSilentDoor pins the backstop's core guarantee: a door that
// never wakes — an attempt it can neither complete nor report on — still ends
// the node at its declared timeout. No test code moves the clock here; the fake
// advances it to the deadline Block was given, which is the boundary a real
// adapter wakes at when its timer fires.
func TestNodeTimeoutSilentDoor(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, "")
	rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
	rt.Hold = map[string]bool{"steps.slow": true}
	res := driveTimeoutRun(t, r, nil)

	n := nodeOf(t, res, "steps.slow")
	if n.State != StateFailed {
		t.Fatalf("state = %s, want failed\nlog:\n%s", n.State, eventLog(res))
	}
	if n.StopReason != StopReasonTimeout {
		t.Errorf("stop reason = %q, want %q: a silent door must still time out", n.StopReason, StopReasonTimeout)
	}
	if res.Status != RunAborted {
		t.Errorf("run status = %s, want aborted", res.Status)
	}
	if rt.Blocks() == 0 {
		t.Errorf("Block was never called: the scheduler did not wait on the door")
	}
}

// TestNodeTimeoutBackoffNotCounted pins that a retry's timeout window starts
// after its backoff, not before: backoff is a wait the scheduler itself honors,
// so it must not eat the next attempt's budget. backoff (10s) exceeds the
// timeout (5s), so a window that included backoff would time the retry out the
// instant it started; the pinned run instead gives the retry a fresh 5s.
func TestNodeTimeoutBackoffNotCounted(t *testing.T) {
	r, rt := newTimeoutRun(t, 5, `      retry:
        max_attempts: 2
        backoff_seconds: 10
`)
	rt.Script = map[string][]Completion{"steps.slow": {Report("never read", 0)}}
	rt.Hold = map[string]bool{"steps.slow": true}
	res := driveTimeoutRun(t, r, pastDeadline(rt, 6*time.Second))

	n := nodeOf(t, res, "steps.slow")
	if n.State != StateFailed {
		t.Fatalf("state = %s, want failed\nlog:\n%s", n.State, eventLog(res))
	}
	if got := len(n.Attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (retry.max_attempts=2)", got)
	}
	// The retry's window starts after its 10s backoff, so the 6s the clock
	// advances past the deadline is the whole elapsed span — not the 16s (6s
	// run plus 10s backoff) a pre-backoff window would report.
	if want := "steps.slow#2 attempt=2 elapsed=6s timeout=5s"; !strings.Contains(eventLog(res), want) {
		t.Errorf("retry log line missing %q — backoff appears to count against the window:\n%s", want, eventLog(res))
	}
}
