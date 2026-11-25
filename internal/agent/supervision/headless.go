// headless.go is the headless (measurement) mode's decider and its tick loop
// (yaah-0nbb, design §4.8): a stateless one-shot completion per tick over the
// rendered snapshot, parsed fail-closed into a per-agent decision list, applied
// through the SAME Batch the agent mode drives.
//
// Design: docs/supervisor-policy-design.md §4.4 (trigger), §4.8 (headless is
// for measurement: no conversation, no tools), §4.12 (authority — the decider
// can call NOTHING: intervention-only authority through the arbiter).
//
// What this file deliberately is not:
//   - it is not a second arbiter. Every decision goes through Batch.Next, so
//     preconditions, cooldowns, the action mask, the intervention cap and the
//     record path are the ones yaah-3l34 built and yaah-vaxv reports. Nothing
//     here applies an intervention directly.
//   - it holds no conversation. Each tick is a fresh call with the locked v1
//     system prompt and one rendered snapshot; nothing carries over but the
//     batch's own label map, which the RENDERER threads (not this decider).
package supervision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	renderer "github.com/goatherdai/yaah/internal/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// DefaultHeadlessTimeout bounds ONE tick's completion (design §4.8). A tick
// that outlives it is a failed tick, not a retry: the batch's next tick renders
// a fresh snapshot, and a decision taken against a 90-second-old one would be
// applied to a scope that has moved on.
const DefaultHeadlessTimeout = 60 * time.Second

// CompletionFunc is one one-shot completion: the locked system prompt and the
// rendered snapshot in, the model's reply text out. It is a seam rather than a
// concrete client so the decider owns no provider plumbing (the composition
// root resolves supervisor.policy_model, exactly as it resolves every other
// model) and a test can stand in a canned reply.
type CompletionFunc func(ctx context.Context, system, snapshot string) (string, error)

// HeadlessDecider is design §4.8's decider: it satisfies
// supervisionapi.Decider (one Snapshot in, one Decision list out) on top of a
// single completion call. It is stateless across ticks by construction — no
// conversation, no tool registry, no clock, and no memory of an earlier reply.
type HeadlessDecider struct {
	// Complete is the model call. Required.
	Complete CompletionFunc

	// Prompt is the locked system prompt — internal/prompts/supervision's
	// MetaPromptV1, selected by supervisor.prompt. Empty disables the decider
	// (Decide returns an error) rather than sending a snapshot with no
	// protocol attached.
	Prompt string

	// BudgetTokens bounds the RENDERED SNAPSHOT (renderer.Render's budget).
	// The caller is responsible for reserving the system prompt's own estimate
	// out of the ~3K the paper's loop spends; <= 0 selects the renderer's
	// default.
	BudgetTokens int

	// Timeout bounds one completion; <= 0 means DefaultHeadlessTimeout.
	Timeout time.Duration

	// Log receives this decider's diagnostics; nil means slog.Default().
	Log *slog.Logger
}

// timeout resolves the per-tick completion deadline.
func (d *HeadlessDecider) timeout() time.Duration {
	if d == nil || d.Timeout <= 0 {
		return DefaultHeadlessTimeout
	}
	return d.Timeout
}

// budget resolves the render budget.
func (d *HeadlessDecider) budget() int {
	if d == nil || d.BudgetTokens <= 0 {
		return renderer.DefaultRenderBudgetTokens
	}
	return d.BudgetTokens
}

// Decide renders one snapshot and asks the model for one decision list
// (supervisionapi.Decider). It is the interface entry point; the tick LOOP
// calls decide directly with the batch's own render, so that the text the model
// read and the label map Batch.Next resolves against are one render (see
// Batch.SnapshotText).
func (d *HeadlessDecider) Decide(ctx context.Context, snap supervisionapi.Snapshot) ([]supervisionapi.Decision, error) {
	if len(snap.Agents) == 0 {
		return nil, nil
	}
	text, labels := renderer.Render(snap, renderer.RenderOptions{BudgetTokens: d.budget()})
	return d.decide(ctx, text, labels)
}

// decide is one tick's model call and parse. An error is the FAIL-CLOSED
// answer — no decisions this tick — never a partial list: a reply whose shape
// is wrong is a reply nobody can read, and applying the half of it that parsed
// would be guessing which worker the model meant.
func (d *HeadlessDecider) decide(ctx context.Context, text string, labels []renderer.Labeled) ([]supervisionapi.Decision, error) {
	if d == nil || d.Complete == nil {
		return nil, errors.New("supervision: the headless decider has no completion function")
	}
	if strings.TrimSpace(d.Prompt) == "" {
		return nil, errors.New("supervision: the headless decider has no meta-prompt (supervisor.prompt)")
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	tctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	reply, err := d.Complete(tctx, d.Prompt, text)
	if err != nil {
		return nil, fmt.Errorf("supervision: the headless decider's completion failed: %w", err)
	}
	return parseDecisions(reply, labels)
}

// Log returns this decider's logger, defaulted.
func (d *HeadlessDecider) logger() *slog.Logger {
	if d == nil || d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// maxDecisionReplyBytes bounds the reply a provider may hand back before the
// parse even looks at it. A decision list is a few lines of JSON; a megabyte of
// prose is not one, and scanning it for a JSON object would cost the tick.
const maxDecisionReplyBytes = 64 << 10

// parseDecisions reads the model's reply as {"decisions": [...]} and validates
// every field, fail-closed (design §4.5, §4.8; the shape the renderer's footer
// instruction asks for, which TestRender_FooterContract pins).
//
// Refused, whole reply dropped:
//   - no JSON object, or no "decisions" array in it (wrong shape);
//   - a field that is present with the wrong type;
//   - an action outside none|steer|redirect|revert — including the rules
//     engine's stop, which a decider may not use (design D6);
//   - a scope that names no label the render assigned;
//   - the same scope twice in one list (one decision per scope per tick).
//
// Accepted and normalized: reason/guidance over their word limits are
// TRUNCATED whole-word (supervisionapi.TruncateWords) rather than refused — the
// arbiter truncates identically, so the text a record carries is the text the
// model meant up to the limit; an explicit action "none" is dropped, because
// leaving a scope alone is what an unlisted scope already means.
func parseDecisions(reply string, labels []renderer.Labeled) ([]supervisionapi.Decision, error) {
	if len(reply) > maxDecisionReplyBytes {
		return nil, fmt.Errorf("supervision: the headless decider's reply is %d bytes, over the %d-byte limit for a decision list", len(reply), maxDecisionReplyBytes)
	}
	obj, ok := decodeReplyObject(reply)
	if !ok {
		return nil, errors.New(`supervision: the headless decider's reply is not a JSON object, so it carries no decision list`)
	}
	raw, ok := obj["decisions"]
	if !ok {
		return nil, errors.New(`supervision: the headless decider's reply has no "decisions" key`)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New(`supervision: the headless decider's "decisions" is not a list`)
	}
	known := make(map[string]bool, len(labels))
	for _, l := range labels {
		known[l.Label] = true
	}
	out := make([]supervisionapi.Decision, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("supervision: decision %d is not an object", i)
		}
		scope, err := decisionField(entry, "scope", i)
		if err != nil {
			return nil, err
		}
		scope = strings.TrimSpace(scope)
		if !known[scope] {
			return nil, fmt.Errorf("supervision: decision %d names scope %q, which the rendered snapshot did not assign (it named %d scope(s): %s)", i, scope, len(labels), labelList(labels))
		}
		actionText, err := decisionField(entry, "action", i)
		if err != nil {
			return nil, err
		}
		var action supervisionapi.Action
		switch strings.ToLower(strings.TrimSpace(actionText)) {
		case string(supervisionapi.ActionNone):
			continue
		case string(supervisionapi.ActionSteer):
			action = supervisionapi.ActionSteer
		case string(supervisionapi.ActionRedirect):
			action = supervisionapi.ActionRedirect
		case string(supervisionapi.ActionRevert):
			action = supervisionapi.ActionRevert
		default:
			return nil, fmt.Errorf("supervision: decision %d names action %q, which is not one of %q, %q, %q or %q", i, actionText, supervisionapi.ActionNone, supervisionapi.ActionSteer, supervisionapi.ActionRedirect, supervisionapi.ActionRevert)
		}
		reason, err := optionalDecisionField(entry, "reason", i)
		if err != nil {
			return nil, err
		}
		guidance, err := optionalDecisionField(entry, "guidance", i)
		if err != nil {
			return nil, err
		}
		if seen[scope] {
			return nil, fmt.Errorf("supervision: decision %d names scope %q a second time; a tick carries at most one decision per scope", i, scope)
		}
		seen[scope] = true
		out = append(out, supervisionapi.Decision{
			Scope:    scope,
			Action:   action,
			Reason:   supervisionapi.TruncateWords(reason, supervisionapi.MaxReasonWords),
			Guidance: supervisionapi.TruncateWords(guidance, supervisionapi.MaxGuidanceWords),
		})
	}
	return out, nil
}

// decisionField reads a required string field.
func decisionField(entry map[string]any, key string, i int) (string, error) {
	v, ok := entry[key]
	if !ok {
		return "", fmt.Errorf("supervision: decision %d has no %q", i, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("supervision: decision %d's %q is not a string", i, key)
	}
	return s, nil
}

// optionalDecisionField reads a field that may be absent, but must be a string
// when present — a null or a number in "reason" is a shape the prompt never
// asked for.
func optionalDecisionField(entry map[string]any, key string, i int) (string, error) {
	v, ok := entry[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("supervision: decision %d's %q is not a string", i, key)
	}
	return s, nil
}

// labelList renders the assigned labels for an error message.
func labelList(labels []renderer.Labeled) string {
	if len(labels) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l.Label)
	}
	return strings.Join(parts, ", ")
}

// decodeReplyObject scans for the first complete JSON object in the reply,
// bounding the attempts. It is the settle_values.go decodeJSONObject rule,
// duplicated rather than imported because that one lives inside the workflow
// engine and internal/supervision must not depend on it: a model that wraps its
// JSON in a sentence or a code fence is the common case the scan exists for.
func decodeReplyObject(s string) (map[string]any, bool) {
	attempts := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '{' {
			continue
		}
		if attempts++; attempts > 128 {
			return nil, false
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var m map[string]any
		if err := dec.Decode(&m); err == nil {
			return m, true
		}
	}
	return nil, false
}

// RunHeadlessLoop drives one batch with one decider until it settles: the
// host-side half of design §4.8.
//
// One iteration is one tick, in the order the batch documents — render once,
// decide, apply:
//
//   - ShouldDecide is the trigger (design §4.4): `always` asks every tick with a
//     live worker, `on-change` only after an ingest that saw a change, and no
//     trigger asks for a tick with no live worker at all.
//   - The renderer's text is the BATCH's (SnapshotText), so the labels the model
//     names are the ones Batch.Next resolves, threaded across ticks by the
//     batch's own label map.
//   - A decider error never ends supervision — a provider hiccup, a timeout or
//     a garbled reply costs one tick's decisions, because the next tick is a
//     fresh call with a fresh snapshot. The failure is logged at WARN once at
//     the start of a failure run and once at its end (with the count); the
//     ticks in between stay at Debug, so a persistently failing provider
//     cannot flood the log at tick cadence.
//   - Next applies the decisions (or just advances the clock), and Done ends
//     the loop: the batch's membership is every live non-root scope, so Done is
//     "there is no worker left to supervise".
//
// The returned error is the reason the loop stopped that is NOT Done: the
// context's cancellation (the dispatch settled, or the session is closing) or a
// batch error. A cancelled context is the normal exit and callers treat it as
// such.
func RunHeadlessLoop(ctx context.Context, batch *Batch, dec *HeadlessDecider, log *slog.Logger) error {
	if batch == nil {
		return errors.New("supervision: the headless loop needs a batch")
	}
	if dec == nil || dec.Complete == nil {
		return errors.New("supervision: the headless loop needs a decider with a completion function")
	}
	if log == nil {
		log = dec.logger()
	}
	// Consecutive failed decide calls, for the rate-limited Warn (finding 6).
	failedTicks := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var decisions []supervisionapi.Decision
		if batch.ShouldDecide() {
			got, err := dec.decide(ctx, batch.SnapshotText(dec.budget()), batch.Labels())
			switch {
			case err != nil && ctx.Err() != nil:
				// The completion was cut off by the loop's own cancellation:
				// that is teardown, not a failed tick.
				return ctx.Err()
			case err != nil:
				// Rate-limited, the noStopOnce rationale: a persistently failing
				// provider would otherwise emit one WARN per tick for the whole
				// dispatch (720/hour at 5s), and the flood is not 720 facts — it
				// is one. The FIRST failure warns in full, the rest stay in
				// Debug, and the first SUCCESS after a failure run warns once
				// with the count, so the operator sees both edges (PR #417
				// review round 2, finding 6).
				failedTicks++
				if failedTicks == 1 {
					log.Warn("supervision: headless tick produced no decisions (fail-closed); the loop continues on the next tick",
						"tick", batch.Tick(), "budget_tokens", dec.budget(), "err", err)
				} else {
					log.Debug("supervision: headless tick produced no decisions (fail-closed)",
						"tick", batch.Tick(), "consecutive_failed_ticks", failedTicks, "err", err)
				}
			default:
				if failedTicks > 0 {
					log.Warn("supervision: headless ticks are deciding again after a failure run",
						"tick", batch.Tick(), "failed_ticks_behind", failedTicks)
					failedTicks = 0
				}
				decisions = got
			}
		}
		res, err := batch.Next(ctx, decisions)
		if err != nil {
			return err
		}
		if res.Done {
			return nil
		}
	}
}

// reservedSystemPromptTokens is the locked v1 meta-prompt's estimated size, by
// the arithmetic internal/supervision/render.go documents: the 4200-byte asset
// is 4200/4 × 1.3 ≈ 1365 estimated tokens. It is duplicated here rather than
// computed because computing it would mean this package importing the prompt
// asset just to measure it — and the reservation is what the caller of the
// renderer owns, which is exactly this mode.
const reservedSystemPromptTokens = 1365

// HeadlessSnapshotBudget is the rendered-snapshot budget headless mode passes to
// the renderer: the paper's ~3K-token prompt MINUS the locked system prompt's
// own reservation, so the pair (system prompt + snapshot) stays inside the
// budget the measurement is comparable at — a snapshot rendered at the
// snapshot-only default would put the pair at ~4.4K, a third over.
const HeadlessSnapshotBudget = renderer.DefaultRenderBudgetTokens - reservedSystemPromptTokens

// HeadlessHost owns a session's headless loop: at most ONE live loop, armed as
// the rules engine's arbiter while it runs, and joined when it stops. It is the
// composition root's handle — the policy decision, the model, the prompt and
// the batch's inputs all come from outside — so that the one thing which must be
// right in every wiring is right in one place (yaah-0nbb, design §4.8).
//
// ONE LOOP PER SESSION, deliberately. The batch's membership is the session
// root's dispatched scopes and its batch IS the shared arbiter (design §4.6), so
// two concurrent loops would mean two batches, one arbiter slot and duplicated
// model calls — and a batch anchored at a single dispatch would have no members
// at all, because a dispatch cannot nest one level deeper (runner depth is 1).
// The dispatch-side seam therefore only says "a dispatch run is starting" and
// "it settled"; the loop's lifetime is bounded by the LAST one, which is the
// only membership that can be supervised.
//
// Lifetime has three bounds, in the order they normally fire: the batch reports
// Done (no live non-root scope left), Settle is called by the last settling
// dispatch, or Stop/Close is called by session teardown. The loop's context is
// deliberately detached from any single dispatch's (context.WithoutCancel in
// Start): one worker being cancelled or halted must not switch supervision off
// for the workers still running.
type HeadlessHost struct {
	// NewBatch builds a fresh batch for a new loop. Called once per Start, not
	// at construction, so a stopped-and-restarted loop begins with a clean tick
	// counter, label map and claim table — the same reason agent mode builds its
	// batch on first use.
	NewBatch func() *Batch

	// Decider is the one-shot decider this loop drives. Required.
	Decider *HeadlessDecider

	// Arm routes the rules engine's firings into the loop's batch and Release
	// puts it back on direct delivery (design §4.6: one arbiter for rules and
	// decisions). Both nil means the session has no rules engine to share, which
	// is legal — the loop then supervises without arbitrating rules.
	Arm     func(RuleArbiter)
	Release func()

	// Log receives the host's diagnostics; nil means slog.Default().
	Log *slog.Logger

	// JoinTimeout bounds how long Stop waits for the loop's goroutine; <= 0
	// means DefaultHeadlessJoinTimeout.
	JoinTimeout time.Duration

	mu  sync.Mutex
	run *headlessRun
}

// DefaultHeadlessJoinTimeout bounds Stop's wait for the loop goroutine. A
// cancelled tick returns at once — Wait selects on the context and the
// completion is deadline-bound — so this only fires if a provider ignores its
// context, and waiting forever there would hang the dispatch's settle.
const DefaultHeadlessJoinTimeout = 15 * time.Second

// headlessRun is one live loop.
type headlessRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	batch  *Batch
	// finished is set the moment the loop's RunHeadlessLoop returns, BEFORE the
	// goroutine re-takes h.mu to clear the slot. It makes the terminal window
	// observable to Start: a run that is finished but not yet cleared must not
	// answer a new dispatch's start with "already running" — that loop is dead,
	// and the dispatch would run unsupervised (PR #417 review round 2,
	// finding 2).
	finished atomic.Bool
}

// StartResult distinguishes the three situations Start finds. The old bool
// folded "a healthy loop is already running" and "this host cannot run any
// loop" into one false, and left the terminal-window case silently
// unsupervised; a caller (RunHook) owes the operator a loud line for the
// second and a fresh loop for the third.
type StartResult int

const (
	// StartStarted means this call armed a fresh loop.
	StartStarted StartResult = iota
	// StartAlreadyRunning means a healthy loop is live and supervising; the
	// dispatch joins its batch's universe as a member.
	StartAlreadyRunning
	// StartCannotRun means no loop exists or was started: the host has no batch
	// constructor or no decider (a construction bug — the composition root
	// refuses to install such a host), or the constructor returned nil.
	StartCannotRun
)

// log returns the host's logger, defaulted.
func (h *HeadlessHost) log() *slog.Logger {
	if h == nil || h.Log == nil {
		return slog.Default()
	}
	return h.Log
}

// joinTimeout resolves the join bound.
func (h *HeadlessHost) joinTimeout() time.Duration {
	if h == nil || h.JoinTimeout <= 0 {
		return DefaultHeadlessJoinTimeout
	}
	return h.JoinTimeout
}

// RunHook is the runner's supervise-run seam: it starts the loop if none is
// running and returns the function the dispatch calls when it SETTLES. The scope
// id is deliberately unused — membership is the session's whole dispatched set,
// not one scope — and is part of the signature so a caller that later wants a
// per-scope bound has it without a second seam.
func (h *HeadlessHost) RunHook(ctx context.Context, _ string) func() {
	if h.Start(ctx) == StartCannotRun {
		// The composition root refuses to install a half-built host, but a host
		// that cannot run a loop must not pass a dispatch through unsupervised
		// without saying so — the same loud-once rule registration follows.
		h.log().Warn("supervision: a supervised dispatch started with NO headless loop — the host has no batch constructor or no decider")
	}
	return func() { h.Settle() }
}

// Start arms the loop if it is not already running. The result distinguishes
// the three situations a caller can land in (see StartResult): a fresh loop
// this call started, a healthy loop already supervising, or a host that cannot
// run any loop. A run that has FINISHED but not yet cleared its slot does not
// answer AlreadyRunning — this call claims the slot and arms a successor, so a
// dispatch starting in that window is supervised rather than silently left
// alone behind a dead loop.
func (h *HeadlessHost) Start(ctx context.Context) StartResult {
	if h == nil || h.NewBatch == nil || h.Decider == nil {
		return StartCannotRun
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run != nil && !h.run.finished.Load() {
		return StartAlreadyRunning
	}
	batch := h.NewBatch()
	if batch == nil {
		return StartCannotRun
	}
	// A FINISHED run still in the slot — the loop ended at Done but its
	// goroutine has not reached the clearing section yet — is claimed here:
	// the clearing is identity-guarded, so it cannot touch this successor, and
	// its Release is skipped because the Arm below already rebinds the single
	// arbiter slot. Without the claim, a dispatch starting in that window gets
	// an "already running" answer from a loop that is dead — silently
	// unsupervised, with the arbiter released under it (PR #417 review round
	// 2, finding 2).
	//
	// WithoutCancel: one dispatch ending must not switch supervision off for the
	// others (see the type comment). Stop, Settle, Done and Close are the bounds.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if h.Arm != nil {
		h.Arm(batch)
	}
	run := &headlessRun{cancel: cancel, done: make(chan struct{}), batch: batch}
	h.run = run
	go h.loop(run, loopCtx)
	return StartStarted
}

// loop is the goroutine: it runs the tick loop, then clears its slot and
// releases the arbiter — unless Stop/Settle already claimed the slot, in which
// case the release is theirs and a second one would disarm a successor loop.
func (h *HeadlessHost) loop(run *headlessRun, ctx context.Context) {
	defer close(run.done)
	// Release the loop context on EVERY exit, including the Done return nobody
	// cancels: the WithCancel resources go away with the goroutine.
	defer run.cancel()
	err := RunHeadlessLoop(ctx, run.batch, h.Decider, h.log())
	// Mark finished BEFORE the clearing section below: from this instant a
	// concurrent Start may claim the slot and arm a successor (see Start), and
	// the identity guard here keeps this goroutine from touching it.
	run.finished.Store(true)
	if err != nil && !errors.Is(err, context.Canceled) {
		h.log().Warn("supervision: the headless loop stopped early", "err", err, "ticks", run.batch.Tick())
	}
	h.mu.Lock()
	if h.run == run {
		h.run = nil
		if h.Release != nil {
			h.Release()
		}
	}
	h.mu.Unlock()
}

// Running reports whether a loop is live.
func (h *HeadlessHost) Running() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run != nil
}

// Settle stops the loop IF its batch has no live worker left, and does nothing
// while one remains. It is what the dispatch-side hook calls when a dispatch
// settles: the common case (the last worker finished) ends the loop at settle
// instead of one interval later, and the concurrent case keeps supervising.
//
// Liveness is Batch.RefreshLive — a FRESH poll, not the cached membership
// Live/ShouldDecide read: at settle time the cache still contains the scope
// that just settled (the run unregisters only after the hook returns), so a
// cached answer would keep the loop alive to its own next Done tick — one
// more interval of model calls the measurement counts, and the arbiter armed
// past the last supervised dispatch (PR #417 review round 2, finding 1).
// RefreshLive, not ShouldDecide, for the round-1 reason too: ShouldDecide
// folds in the trigger, and under TriggerOnChange a quiet tick would stop the
// loop while siblings are still live.
//
// The membership read and the slot claim happen under ONE lock, so a new
// dispatch starting at that instant either sees a live loop (and settles into it
// on its own) or starts a fresh one — it can never be left supervised by a loop
// that is already being torn down (PR #417 review finding 2). Only the cancel
// and the join run outside the lock: they must not hold it while a goroutine
// winds down. Lock order: host-mu BEFORE batch-mu (RefreshLive polls under
// this lock); nothing in the package takes them the other way round.
func (h *HeadlessHost) Settle() {
	if h == nil {
		return
	}
	h.mu.Lock()
	run := h.run
	if run == nil {
		h.mu.Unlock()
		return
	}
	if run.batch.RefreshLive() {
		h.mu.Unlock()
		return
	}
	run = h.claimLocked()
	h.mu.Unlock()
	h.join(run)
}

// claimLocked detaches the current run and releases the arbiter under the
// caller's held lock: a concurrent Start then either sees this run or an
// already-cleared slot it can arm cleanly, and the stopping goroutine's own
// release is skipped (it no longer owns the slot). Nil means no loop ran.
func (h *HeadlessHost) claimLocked() *headlessRun {
	run := h.run
	if run == nil {
		return nil
	}
	h.run = nil
	if h.Release != nil {
		h.Release()
	}
	return run
}

// join cancels the run's loop context and waits for its goroutine to exit.
// Nil is a no-op. Called OUTSIDE h.mu: the loop goroutine may touch the host
// while winding down, and a bounded join must not hold the lock it needs.
func (h *HeadlessHost) join(run *headlessRun) {
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(h.joinTimeout()):
		h.log().Warn("supervision: the headless loop did not join within its timeout; its goroutine is still running",
			"timeout", h.joinTimeout())
	}
}

// Stop cancels the loop, joins its goroutine and releases the arbiter. It is
// idempotent and safe to call with no loop running.
func (h *HeadlessHost) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	run := h.claimLocked()
	h.mu.Unlock()
	h.join(run)
}

// Close is session teardown's entry point: it stops and joins any live loop. It
// exists as a name because teardown reads better saying what it means, and
// because the ordering constraint it shares with the supervise tool's release
// (release the arbiter BEFORE the rules service closes) is worth stating at the
// call site.
func (h *HeadlessHost) Close() { h.Stop() }
