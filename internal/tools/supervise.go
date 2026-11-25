package tools

// supervise.go is agent mode's meta-agent loop surface (yaah-fy28, design
// §4.7): the `supervise` tool. The dispatching agent's own turn cycle IS the
// meta-agent loop — its model call reads the snapshot this tool returned and
// its next `supervise` call delivers the decisions — so the host owns the
// tick and the tool call is the tick boundary. One dispatching-agent model
// call per tick is the price of the mode.
//
// The tool is a CLIENT of the shared arbiter, never a decider: it hands the
// decision list to Batch.Next, which is the same step function the headless
// measurement loop (yaah-0nbb) and the phase-2 workflow node will call. The
// tool therefore computes no opinion of its own — it validates the argument
// shape, binds the batch, calls the step function, and reports exactly what
// happened, drops included.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// errSuperviseBusy reports a call that arrived while another call on the same
// tool instance was in flight. It never leaves this file: Execute turns it into
// the structured busy reply rather than an error, because a busy tick is a
// scheduling fact about one session, not a failed call.
var errSuperviseBusy = errors.New("supervise: a call is already in flight for this session")

// The two wait modes (design §4.7). tick is the default: one model call per
// tick is what makes the agent's turn cycle the loop.
const (
	// superviseWaitTick applies the decisions, waits for the next tick with
	// zero model turns, and returns the snapshot the next decision list
	// should read.
	superviseWaitTick = "tick"
	// superviseWaitSettled applies the decisions and then blocks (a Next loop,
	// still with zero model turns) until every scope in the batch has
	// finished, returning the intervention records — the payload a
	// background dispatch collects today.
	superviseWaitSettled = "settled"
)

// A wait:"settled" call's bounds (design §4.7). "Settled" means settled, but a
// settle that cannot finish must still return: Batch.Next never fails on scope
// state — only a cancelled ctx ends it — so one worker that never finishes would
// make every settle call an unbounded block that spends the model's whole tool
// budget on a single turn, and its tick list would grow without limit beside it.
const (
	// maxSettleTicks bounds how many ticks one settle call runs. The bound is
	// stated in TICKS rather than in seconds so it is deterministic and needs no
	// clock seam; a tick is one Batch interval (5 seconds at the documented
	// default), so this is about ten minutes of wall clock — long enough for a
	// real dispatch batch, short enough that an abandoned one gives the turn back.
	maxSettleTicks = 120

	// maxReportedTicks bounds the per-tick records a reply retains. A settle
	// accumulates one TickResult per tick, and a reply carrying all of them plus
	// the snapshot would spend the caller's context on history it can no longer
	// act on. The most RECENT ticks are kept — those are what a caller deciding
	// its next move reads — and the reply's `ticks` count still reports every tick
	// the call ran, with a note stating how many earlier records were dropped.
	maxReportedTicks = 32
)

// superviseSchema is the tool's argument schema. It is a function rather than
// a constant so the property list exists in exactly one place a test can
// read; the embedded doc names every property (doc_parity_test.go).
func superviseSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"decisions": {
				"type": "array",
				"description": "Per-agent decisions for the snapshot the last call returned. At most one decision per scope; an unlisted scope is left alone (none). Omit for a tick that only observes.",
				"items": {
					"type": "object",
					"properties": {
						"scope": {"type": "string", "description": "Scope label exactly as the snapshot renders it (w1, w2, ...) or a raw scope id (scope:sub-a). A label wins when a string is somehow both."},
						"action": {"type": "string", "enum": ["none", "steer", "redirect", "revert"], "description": "none leaves the scope alone; steer is cheap (the run keeps its conversation); redirect is expensive (fresh loop, memory lost); revert is expensive AND discards the scope's workspace changes."},
						"reason": {"type": "string", "description": "Why, at most 20 words."},
						"guidance": {"type": "string", "description": "What to tell the scope, at most 60 words. Required for steer, redirect and revert; the successor's first message for redirect and revert."}
					},
					"additionalProperties": false,
					"required": ["scope", "action"]
				}
			},
			"wait": {
				"type": "string",
				"enum": ["tick", "settled"],
				"description": "tick (default): apply the decisions, wait for the next tick, return the new snapshot. settled: apply them, then block until every supervised scope has finished and return the intervention records."
			}
		},
		"additionalProperties": false
	}`)
}

// SuperviseBatch is the tool's consumer-side view of the supervisor policy
// loop's step function (design §4.1). The concrete *supervision.Batch
// satisfies it structurally, so this package never imports the agent family
// back (the same boundary SnapshotSource and runBridge keep).
type SuperviseBatch interface {
	// Next runs one tick: validate and apply the decisions against the last
	// ingested snapshot, wait one interval with no model turns, ingest, and
	// report what became of every decision.
	Next(ctx context.Context, decisions []supervisionapi.Decision) (supervisionapi.TickResult, error)
	// SnapshotText renders the last ingested snapshot as meta-agent text,
	// assigning the w1..wN labels a decision names.
	SnapshotText(budgetTokens int) string
}

// SuperviseBinding is one batch plus the arbiter pairing hooks that keep the
// rules engine and this tool on the SAME step function (design §4.6). The
// composition root builds it; the tool owns when it is armed and released.
//
// The hooks are needed because one arbiter serving both halves has a lifetime
// the batch alone cannot express: the rules engine must route into the batch
// WHILE it ticks, and must fall back to direct delivery when the loop stops —
// otherwise later rule firings queue into a batch nothing drives and drop past
// its queue cap, silently degrading the session's rules surface. Arm and
// Release are the two ends of that lifetime; either may be nil (no rules
// engine wired, in which case there is nothing to bind or unbind).
type SuperviseBinding struct {
	// Batch is the batch the tool ticks. Nil is a binding failure.
	Batch SuperviseBatch
	// Arm routes the session's rules engine into Batch. The tool calls it
	// when it binds and again after every Release.
	Arm func()
	// Release restores the rules engine's direct delivery. The tool calls it
	// once a tick reports Done (the loop has nothing left to drive) and
	// re-arms on the next call if that batch is reused.
	Release func()
}

// SuperviseBatchFactory builds the batch bound to one calling scope. The
// composition root supplies it: it owns the run registry, the snapshot
// source, the stop delivery function and the arbiter pairing, none of which
// this package can reach. The calling scope is passed in rather than captured
// so the batch's descendant filter and its never-a-target guard both name the
// caller the tool actually bound.
type SuperviseBatchFactory func(callerScopeID string) *SuperviseBinding

// SuperviseTool is the `supervise` tool: agent mode's tick surface.
//
// Binding: the first call binds ONE batch to the calling loop's scope. One
// batch per calling loop, so a second caller is refused — a second batch on
// the same session would give two deciders one shared claim table, which is
// the interleaving ErrBatchBusy exists to prevent within a batch and which
// only binding can prevent across them.
type SuperviseTool struct {
	// CallerScopeID is the scope whose descendants this loop supervises — the
	// dispatching agent's own scope, set by the wiring (the session root in
	// agent mode). The root scope is never in the batch: the batch's
	// membership is the caller's STRICT descendants.
	CallerScopeID string

	// CallerScope optionally resolves the calling loop's scope for one call,
	// overriding CallerScopeID. It is the seam for a nested dispatching agent
	// (a sub-agent that dispatches): the runner would carry the calling
	// scope's id on the tool context. Nil — today's wiring — means every call
	// resolves to CallerScopeID, which is exactly right for the root agent.
	CallerScope func(ctx context.Context) string

	// NewBatch builds the batch on first use. Nil means the supervision
	// engine is unavailable in this session and every call reports that
	// instead of pretending to supervise.
	NewBatch SuperviseBatchFactory

	// BudgetTokens bounds the rendered snapshot's size; 0 selects the
	// renderer's default (the snapshot-only budget, design §4.3).
	BudgetTokens int

	// mu guards the binding and the single-flight flag. It is held across the
	// bookkeeping only, never across a tick — a tick can block for a whole
	// interval (or, on a settle, for the batch's life), so holding this lock
	// across it would make every concurrent call wait instead of being told
	// the session is busy.
	//
	// inFlight makes the tool ONE CALL AT A TIME PER SESSION, which is what
	// keeps the render inside the tick's single flight: SnapshotText assigns
	// the w1..wN labels the NEXT decision list resolves against, so a second
	// call rendering while another ticks would repoint the labels under it.
	// A concurrent call is refused with the busy reply (nothing applied, no
	// tick, retry next turn) rather than queued behind the lock.
	//
	// ErrBatchBusy stays the batch's own guard: a second tick driver that
	// shares the batch without going through this tool (the headless decider)
	// is refused by the batch, and Execute reports that as the same busy
	// reply.
	mu          sync.Mutex
	inFlight    bool
	boundCaller string
	batch       SuperviseBatch
	// arm and release are the binding's arbiter hooks, armed tracks whether
	// the rules engine is currently routed into batch.
	arm     func()
	release func()
	armed   bool
	// closed reports that the session tore the tool down (Close). A closed tool
	// refuses calls instead of re-arming an arbiter the teardown already
	// released: a wake-initiated turn racing session shutdown must not route the
	// rules engine back into a batch no call will ever tick again.
	closed bool
}

func (*SuperviseTool) Name() string { return "supervise" }

func (*SuperviseTool) Description() string {
	return prompts.ToolDescription("supervise")
}

func (*SuperviseTool) Schema() json.RawMessage { return superviseSchema() }

func (t *SuperviseTool) Execute(ctx context.Context, args string) (string, error) {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	strict, err := DecodeStrictObject("supervise", args, "decisions", "wait")
	if err != nil {
		return "", err
	}
	decisions, err := decodeSuperviseDecisions(strict["decisions"])
	if err != nil {
		return "", err
	}
	wait, err := decodeSuperviseWait(strict["wait"])
	if err != nil {
		return "", err
	}

	batch, err := t.begin(ctx)
	if err != nil {
		if errors.Is(err, errSuperviseBusy) {
			return encodeSuperviseResult(superviseBusyResult())
		}
		return "", err
	}

	var out superviseResult
	if wait == superviseWaitSettled {
		out, err = t.settle(ctx, batch, decisions)
	} else {
		out, err = t.tick(ctx, batch, decisions)
	}
	// The flight ends with the call, and the render inside result() is inside
	// it: the text this reply carries is the text the caller's next decision
	// list is made from, so no other call on this session may render (and
	// repoint the labels) between the tick and the reply.
	// One locked section ends the flight AND releases the arbiter when the call
	// is not leaving a driven batch behind. Done is not the only way a supervise
	// loop ends: a cancelled tick (Ctrl-C, an exhausted turn budget, a session
	// shutting down) leaves the batch live but undriven, and leaving the rules
	// engine routed into it would queue firings into a batch nothing ticks — so
	// a call that did not complete releases too, and the next call re-arms
	// (begin). The release rides inside end's critical section because the two
	// apart are a race: a concurrent Execute could re-arm between them, and this
	// call's release would then detach the OTHER call's arbiter (review M1-r2).
	t.end(out.Done || err != nil)
	if err != nil {
		return "", err
	}
	return encodeSuperviseResult(out)
}

// encodeSuperviseResult renders the reply as the documented JSON shape.
func encodeSuperviseResult(out superviseResult) (string, error) {
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("supervise: encode result: %w", err)
	}
	return string(encoded), nil
}

// tick is the default mode: one tick, then the snapshot the next decision
// list should read.
func (t *SuperviseTool) tick(ctx context.Context, batch SuperviseBatch, decisions []supervisionapi.Decision) (superviseResult, error) {
	res, err := batch.Next(ctx, decisions)
	if err != nil {
		if errors.Is(err, supervisionapi.ErrBatchBusy) {
			// Another tick driver owns the batch. Nothing of this call ran, so
			// the honest reply is the busy result and not a failed turn.
			return superviseBusyResult(), nil
		}
		return superviseResult{}, fmt.Errorf("supervise: the tick did not complete: %w", err)
	}
	return t.result(batch, []supervisionapi.TickResult{res}, 1), nil
}

// settle applies the decisions and then ticks until the batch reports Done.
// Every tick is recorded, so the reply is the whole intervention history —
// the accounting a caller makes a decision about afterwards.
//
// The loop is bounded by ctx and by the settle cap, not by the model's
// patience: "settled" means settled, but a batch that cannot settle must still
// hand the turn back (see maxSettleTicks). A cancelled ctx returns the error
// with the records already collected named in it, so nothing that happened is
// lost from the report; a tripped cap returns the same partial payload as a
// RESULT, done:false, with the cap stated in its note — the batch is still live
// and nothing went wrong, so an error would blame the call for the batch's
// shape.
//
// The counts folded into that error are the partial payload's, honestly: an
// interrupted settle is still an error (the loop did not finish), and the
// agent loop replaces a tool result string with the error text, so a JSON body
// returned beside an error would never reach the model. The error therefore
// states the ticks that ran, the records they made, what took effect, and the
// interruption itself.
func (t *SuperviseTool) settle(ctx context.Context, batch SuperviseBatch, decisions []supervisionapi.Decision) (superviseResult, error) {
	ticks := make([]supervisionapi.TickResult, 0, 4)
	next := decisions
	// ran is how many ticks this call ran, which is not len(ticks) once the
	// retained window truncates.
	ran := 0
	for {
		// Cancellation between ticks: the batch refused nothing, so this is the
		// same interruption as a cancelled wait, reported with the records the
		// completed ticks really made.
		if err := ctx.Err(); err != nil {
			return superviseResult{}, settleInterrupted(t.result(batch, ticks, ran), err)
		}
		res, err := batch.Next(ctx, next)
		if err != nil {
			if errors.Is(err, supervisionapi.ErrBatchBusy) && ran == 0 {
				// Nothing of this call ran: another tick driver already holds the
				// batch. That is the scheduling fact the plain-tick path reports as
				// the busy reply, so report it the same way — as a failure it would
				// read as an internal error and tell the model to wait for the very
				// call it just made. A busy AFTER a completed tick is different: the
				// external driver took the batch between this call's ticks, so it is
				// an interruption and the records already made are kept.
				return superviseBusyResult(), nil
			}
			// Only count a tick that actually carries something: a refused
			// Next returns the zero TickResult and did no work, so counting it as a
			// tick of this call would inflate the report.
			if len(res.Results) > 0 || res.Done {
				ran++
				ticks = retainTick(ticks, res)
			}
			return superviseResult{}, settleInterrupted(t.result(batch, ticks, ran), err)
		}
		ran++
		ticks = retainTick(ticks, res)
		if res.Done {
			return t.result(batch, ticks, ran), nil
		}
		if ran >= maxSettleTicks {
			out := t.result(batch, ticks, ran)
			out.Done = false
			out.addNote(fmt.Sprintf("wait:\"settled\" stopped at the settle cap of %d ticks with the batch still live; the records above are what this call saw — call again with wait:\"settled\" to keep waiting, or wait:\"tick\" to decide from this snapshot", maxSettleTicks))
			return out, nil
		}
		// Only the first tick carries decisions: the rest are pure waits, and
		// the caller's decision for a later tick arrives on its next call.
		next = nil
	}
}

// settleInterrupted states an interrupted settle: an error, because the loop
// did not finish, naming the ticks that ran, the records they made and what
// took effect so the model still sees what its decisions triggered.
func settleInterrupted(partial superviseResult, err error) error {
	return fmt.Errorf(
		"supervise: waiting for the batch to settle was interrupted after %d tick(s) with %d decision record(s) and %d applied intervention(s)%s: %w",
		partial.Ticks, len(partial.Results), len(partial.Applied), appliedSummary(partial.Applied), err)
}

// retainTick appends one tick's result to the retained history, keeping the most
// recent maxReportedTicks. It drops the OLDEST because a settle reply is read to
// learn what just happened: the records that matter for the caller's next
// decision are the latest ones, and the reply's `ticks` count still reports every
// tick the call ran (result).
func retainTick(ticks []supervisionapi.TickResult, res supervisionapi.TickResult) []supervisionapi.TickResult {
	if len(ticks) < maxReportedTicks {
		return append(ticks, res)
	}
	copy(ticks, ticks[1:])
	ticks[len(ticks)-1] = res
	return ticks
}

// result projects the ticks into the tool's reply shape. The snapshot text is
// rendered ONCE here, after the last tick, which is the render-once-per-tick
// convention of design §4.5 read from the other end: the text this reply
// carries is the text the caller's next decision list is made from, and the
// next Next call resolves labels against that same render.
//
// ran is how many ticks the call ran — Ticks reports that, not len(ticks), so a
// reply whose retained window dropped the earliest ticks still reports the work
// the call actually did, and says so in its note.
func (t *SuperviseTool) result(batch SuperviseBatch, ticks []supervisionapi.TickResult, ran int) superviseResult {
	out := superviseResult{
		Applied: make([]superviseApplied, 0, 4),
		Results: make([]superviseRecord, 0, 4),
		Ticks:   ran,
	}
	for _, tick := range ticks {
		for _, rec := range tick.Applied {
			out.Applied = append(out.Applied, superviseApplied{
				Scope:   rec.Scope,
				Action:  string(rec.Action),
				Outcome: rec.Outcome,
			})
		}
		out.Results = append(out.Results, superviseRecords(tick.Results)...)
		out.Done = tick.Done
	}
	out.Snapshot = batch.SnapshotText(t.BudgetTokens)
	if dropped := ran - len(ticks); dropped > 0 {
		out.addNote(fmt.Sprintf("%d earlier tick(s) are not repeated here: a reply keeps the most recent %d ticks' records (of %d run)", dropped, maxReportedTicks, ran))
	}
	return out
}

// callerID resolves the calling loop's scope for this call: the ctx seam when
// the runner carries one, otherwise the configured session root.
func (t *SuperviseTool) callerID(ctx context.Context) string {
	caller := strings.TrimSpace(t.CallerScopeID)
	if t.CallerScope != nil {
		if resolved := strings.TrimSpace(t.CallerScope(ctx)); resolved != "" {
			caller = resolved
		}
	}
	return caller
}

// begin opens the call's single flight: it resolves the calling scope, binds
// the batch on the first call, re-arms a released arbiter, and refuses a
// concurrent call on the same session.
//
// A different caller is refused: one batch per calling loop. The refusal is
// what keeps a second dispatching loop — a sub-agent that dispatches, whose
// ctx seam resolves its own scope — from joining the root's batch and
// steering its siblings; a caller whose scope cannot be resolved at all is
// refused for the same reason, since there is nothing to bind it to.
func (t *SuperviseTool) begin(ctx context.Context) (SuperviseBatch, error) {
	caller := t.callerID(ctx)
	if caller == "" {
		return nil, fmt.Errorf("supervise: no calling scope is configured for this session, so there is nothing to bind the batch to; supervisor.policy: agent needs a session root scope")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		// A closed tool must not re-arm: a wake-initiated turn racing session
		// teardown would otherwise route the rules engine back into a batch no
		// call will ever tick again.
		return nil, fmt.Errorf("supervise: this session's supervision was torn down (the session is closing), so no tick can be run")
	}
	if t.inFlight {
		return nil, errSuperviseBusy
	}
	if t.batch != nil {
		if t.boundCaller != caller {
			return nil, fmt.Errorf("supervise: this session's batch is already bound to the descendants of %s; one batch per calling loop, so %s cannot supervise the batch another loop is deciding for", t.boundCaller, caller)
		}
		// Done can be TRANSIENT: a dispatch that appears after the batch
		// settled re-joins the same batch, so a released arbiter is re-armed
		// here rather than being left in direct delivery while this tool
		// ticks the very batch the rules should be routing into.
		t.armLocked()
		t.inFlight = true
		return t.batch, nil
	}
	if t.NewBatch == nil {
		return nil, fmt.Errorf("supervise: the supervision engine is unavailable in this session (no batch factory is wired), so no tick can be run")
	}
	binding := t.NewBatch(caller)
	if binding == nil || binding.Batch == nil {
		return nil, fmt.Errorf("supervise: the supervision engine could not bind a batch for %s", caller)
	}
	t.batch, t.boundCaller = binding.Batch, caller
	t.arm, t.release = binding.Arm, binding.Release
	t.armLocked()
	t.inFlight = true
	return t.batch, nil
}

// end closes the call's flight, and — in the SAME critical section — releases
// the arbiter pairing when the call is not leaving a driven batch behind. Both
// triggers release: a call that reported Done (the loop is over, so the rules
// engine goes back to direct delivery instead of queueing firings into a batch
// nothing ticks) and a call that failed or was cancelled (the batch is live but
// undriven until the next call, which re-arms). One lock section for flight and
// release is the point: two separate ones let a concurrent Execute re-arm in
// between, and this call's release would detach the other call's arbiter. The
// batch itself is kept — the next call re-arms it — because Done is a statement
// about the scopes it can currently see, not about the session.
func (t *SuperviseTool) end(release bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if release {
		t.releaseLocked()
	}
	t.inFlight = false
}

// Close is the tool's teardown seam: it hands the session's rules engine back to
// direct delivery, and refuses every later call.
//
// It exists because Done is not the only way a supervise loop ends. A model can
// simply stop calling the tool — an abandoned plan, an exhausted turn budget, a
// session shutting down with workers still live — and Service.SetArbiter(batch)
// routing would then persist forever: rule firings queue into a batch nothing
// ticks and drop past its queue cap (the failure supervise_batch.go documents),
// silently degrading the session's rules surface. The session teardown calls
// this before it closes the rules service, so the service's own drain delivers
// directly instead of queueing into a dead batch.
//
// It releases the routing and nothing else: it does not wait for an in-flight
// call (the flight holds no lock across a tick, so there is nothing to wait on)
// and it does not close the batch (a batch has no Close, and its scopes belong
// to the session). Idempotent.
func (t *SuperviseTool) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseLocked()
	t.closed = true
}

// armLocked routes the rules engine into the bound batch, if it is not
// already routed there. A nil Arm means no rules engine is wired.
func (t *SuperviseTool) armLocked() {
	if t.armed || t.arm == nil {
		return
	}
	t.arm()
	t.armed = true
}

// releaseLocked restores the rules engine's direct delivery. A nil Release
// means there is nothing to restore.
func (t *SuperviseTool) releaseLocked() {
	if !t.armed || t.release == nil {
		return
	}
	t.release()
	t.armed = false
}

// superviseResult is the tool's reply: done, the rendered snapshot, what took
// effect, and every decision's record (drops included, with their reason).
type superviseResult struct {
	// Done reports that every scope in the batch has finished.
	Done bool `json:"done"`
	// Busy reports that another call on this session was already in flight
	// (or another tick driver held the batch), so this call applied nothing
	// and ran no tick. Note says what to do about it.
	Busy bool `json:"busy"`
	// Snapshot is the meta-agent snapshot text of the last ingested snapshot.
	Snapshot string `json:"snapshot"`
	// Applied holds the decisions that took effect, in tick order.
	Applied []superviseApplied `json:"applied"`
	// Results holds every decision this call's ticks saw with its outcome,
	// including the dropped ones — a decision that did nothing is reported,
	// never silently discarded.
	Results []superviseRecord `json:"results"`
	// Ticks is how many ticks this call ran (1 for wait: "tick"), so a
	// settle reports how long the batch took in ticks.
	Ticks int `json:"ticks"`
	// Note carries one line of context when the reply needs it — today only
	// the busy reply, which has no snapshot to report.
	Note string `json:"note,omitempty"`
}

// superviseBusyResult is the reply for a call that arrived while another call
// on this session was in flight: nothing applied, no tick run, and the reason
// stated instead of a failed turn.
func superviseBusyResult() superviseResult {
	return superviseResult{
		Busy:    true,
		Applied: make([]superviseApplied, 0),
		Results: make([]superviseRecord, 0),
		Note:    "another supervise call was already in flight for this session, so this call applied nothing and ran no tick; use the snapshot from the reply that was in flight and call again on your next turn",
	}
}

// addNote joins one line of context onto the reply's note, so two facts that
// both need stating — a retained-window truncation and a settle cap — do not
// overwrite each other.
func (out *superviseResult) addNote(line string) {
	if line == "" {
		return
	}
	if out.Note == "" {
		out.Note = line
		return
	}
	out.Note += " — " + line
}

// maxErrAppliedDetails caps how many applied interventions an interrupted
// settle's error names, so the error stays one readable line.
const maxErrAppliedDetails = 5

// appliedSummary states what an interrupted settle managed to apply, so the
// model still sees what its decisions triggered.
func appliedSummary(applied []superviseApplied) string {
	if len(applied) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" — applied: ")
	for i, a := range applied {
		if i == maxErrAppliedDetails {
			fmt.Fprintf(&b, ", (+%d more)", len(applied)-maxErrAppliedDetails)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s %s", a.Scope, a.Action, a.Outcome)
	}
	return b.String()
}

// superviseApplied is one decision that took effect. The outcome says how far
// it got: applied (a steer landed) or claimed (a redirect or revert claimed
// the run for a handoff the runner's adoption loop completes).
type superviseApplied struct {
	Scope   string `json:"scope"`
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
}

// superviseRecord is one decision with its outcome — the tool-facing form of
// supervisionapi.InterventionRecord.
type superviseRecord struct {
	Tick       int    `json:"tick"`
	Scope      string `json:"scope"`
	Source     string `json:"source"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
	Guidance   string `json:"guidance,omitempty"`
	Outcome    string `json:"outcome"`
	DropReason string `json:"drop_reason,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
}

func superviseRecords(recs []supervisionapi.InterventionRecord) []superviseRecord {
	if len(recs) == 0 {
		return nil
	}
	out := make([]superviseRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, superviseRecord{
			Tick:       rec.Tick,
			Scope:      rec.Scope,
			Source:     rec.Source,
			Action:     string(rec.Action),
			Reason:     rec.Reason,
			Guidance:   rec.Guidance,
			Outcome:    rec.Outcome,
			DropReason: rec.DropReason,
			Generation: rec.Generation,
		})
	}
	return out
}

// decodeSuperviseDecisions decodes the decision list into the arbiter's own
// vocabulary. Unknown keys are refused at this level too: the top-level strict
// check cannot see inside the array, and a decision with a misspelled key
// ("guidence") would otherwise reach the arbiter with empty guidance and be
// dropped as empty_guidance — the drop blamed on the action rather than on the
// argument that was actually wrong.
//
// It validates SHAPE — keys and types — and deliberately leaves the prompt's
// remaining invariants to the one implementation that already enforces them, the
// arbiter:
//
//   - "at most one decision per scope": two decisions naming one scope are
//     resolved by ACTION RANK (revert > redirect > steer), and the loser is
//     recorded as a duplicate rather than dropped in silence — the same
//     precedence rule the tick's two halves use (design §4.6). Refusing the list
//     here would turn a ranked, reported outcome into a failed turn and take a
//     capability away from a caller that meant to escalate.
//   - the word caps on reason and guidance: the arbiter truncates both to whole
//     words at the record and delivery boundary (batch.go truncateWords, bounded
//     by supervisionapi.MaxReasonWords and MaxGuidanceWords), so an oversized
//     string never reaches a worker or a record verbatim.
//
// A scope id, an action verb and a missing guidance are likewise the arbiter's to
// judge (stale, unknown_action, empty_guidance): a decoder cannot know which
// names resolve against the render the model read.
func decodeSuperviseDecisions(raw json.RawMessage) ([]supervisionapi.Decision, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []struct {
		Scope    string `json:"scope"`
		Action   string `json:"action"`
		Reason   string `json:"reason"`
		Guidance string `json:"guidance"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("supervise: invalid decisions: %w — each decision accepts exactly scope, action, reason and guidance", err)
	}
	out := make([]supervisionapi.Decision, 0, len(items))
	for _, it := range items {
		out = append(out, supervisionapi.Decision{
			Scope:    strings.TrimSpace(it.Scope),
			Action:   supervisionapi.Action(strings.TrimSpace(it.Action)),
			Reason:   it.Reason,
			Guidance: it.Guidance,
		})
	}
	return out, nil
}

// decodeSuperviseWait parses the wait mode. An absent wait is the default
// (tick); anything outside the two values is refused here rather than being
// guessed at, because the difference between them is a blocked turn versus a
// returned snapshot.
func decodeSuperviseWait(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return superviseWaitTick, nil
	}
	var wait string
	if err := json.Unmarshal(raw, &wait); err != nil {
		return "", fmt.Errorf("supervise: wait must be a string: %w", err)
	}
	switch mode := strings.ToLower(strings.TrimSpace(wait)); mode {
	case "", superviseWaitTick:
		return superviseWaitTick, nil
	case superviseWaitSettled:
		return superviseWaitSettled, nil
	default:
		return "", fmt.Errorf("supervise: wait %q is invalid; use %q (default, one tick then the snapshot) or %q (block until every supervised scope has finished)", wait, superviseWaitTick, superviseWaitSettled)
	}
}
