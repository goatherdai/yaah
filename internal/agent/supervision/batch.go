// batch.go runs the supervisor policy loop's tick (yaah-3l34): one Batch.Next
// call is one tick — apply the decisions the caller's decider returned for the
// last snapshot, wait the interval with zero model turns, ingest, and report
// what became of every decision.
//
// Design: docs/supervisor-policy-design.md §4.1 (one step function, three
// callers), §4.4 (trigger), §4.5 (decider binding), §4.6 (one arbiter for
// rules and decisions).
//
// Batch is the SHARED ARBITER, not the caller's decider. It owns no model and
// forms no opinion: the caller renders the snapshot (SnapshotText), has its
// decider act on that text, and hands the decision list back to Next. The same
// arbiter serves three callers — the `supervise` tool (agent mode), the
// headless one-shot decider, and (phase 2) a `fanout` workflow node — which is
// what makes the two measured modes comparable at all.
//
// What this file deliberately does NOT do, and which bead adds it:
//   - the `supervise` tool surface and its embedded doc (yaah-fy28);
//   - the headless Decider plus `supervisor.policy`/`policy_model` config and
//     the locked v1 prompt wiring (yaah-0nbb; the prompt asset and the
//     renderer are landed, the binding is not);
//   - the dispatch-envelope records that cite TickResult (yaah-vaxv);
//   - revert by construction — lane upgrade, fatal pre-run capture — which is
//     what makes a revert reachable on a dispatched scope at all (yaah-ugtl).
//     Until that lands, a revert is only as good as the registration says
//     (Registration.Reversible), which is exactly what this arbiter reads: a
//     revert on a scope with no capture is dropped, never downgraded.
package supervision

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	renderer "github.com/goatherdai/yaah/internal/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// Batch cadence and tuning defaults. They are the config defaults the design
// fixes (§4.4, §4.6); a zero BatchConfig field selects them rather than
// meaning "off", so an unconfigured batch still behaves like the paper's loop.
const (
	// DefaultBatchInterval is the tick cadence (supervisor.interval's 5s).
	DefaultBatchInterval = 5 * time.Second
	// DefaultCooldownTicks is the default cooldown: a scope acted on in tick
	// N ignores a new action in tick N+1. Zero in BatchConfig.CooldownTicks
	// disables the cooldown outright, which is why the default is applied
	// only to a NEGATIVE value.
	DefaultCooldownTicks = 1
)

// ErrBatchBusy reports a Next call while another Next is still running. One
// batch drives one tick at a time: the caller's decider is a per-tick
// conversation with a tick counter and a claim table behind it, so a second
// concurrent tick would interleave two deciders' decisions against the same
// state. The refused caller (design §5: "a second caller is refused") gets
// this error and retries on its next turn.
var ErrBatchBusy = errors.New("supervision: a tick is already in progress for this batch")

// ruleSteerReason is the reason a queued rules steer carries. The kernel's
// Intervention (supervisor.Intervention) names its type, scope and payload but
// not the rule that fired, so the queue cannot restate the rule here; the
// Service's Firings view (adoption plan B4) keeps the rule name beside the
// count, and that is where it is read.
const ruleSteerReason = "advisory rule fired"

// ageJustActed is the cooldown age recorded at the moment an intervention
// lands. Ages are bumped once at the end of every tick, so an intervention
// made in tick N is age 0 by the end of tick N, 1 in tick N+1, and so on; a
// scope is inside a cooldown of K ticks while age < K, which is exactly "the
// K ticks after the one that acted". The rendered ticks_ago is age+1.
const ageJustActed = -1

// Trigger selects when the caller's decider should run (design §4.4).
//
// It is a CALLER hint, and Batch never silently discards decisions because of
// it: the design ties the trigger to the DECIDER INVOCATION, and the caller
// owns the decider. A caller asks ShouldDecide and skips its model call when
// the answer is false; decisions it supplies anyway are validated and applied
// normally, so a decision is never dropped for a reason the caller cannot see.
//
// The design's third value, `never`, is `policy: none`: no batch is
// constructed at all, so there is no Trigger for it here.
type Trigger string

const (
	// TriggerAlways ticks every interval that has a live agent — the
	// Appendix E protocol, and the default.
	TriggerAlways Trigger = "always"
	// TriggerOnChange ticks only when the ingest that ended the previous tick
	// saw a change (Batch.Changed): a scope advanced its Through cursor,
	// errored, or named a file another scope in the batch also named.
	TriggerOnChange Trigger = "on-change"
)

// StopFunc delivers a rules stop — the Service's deliverHalt path, injected so
// this bead does not rewire Service internals. delivered reports whether the
// stop actually landed; false is a REFUSAL the delivery side made on purpose (a
// root scope without supervisor.halt_root, a merged scope, a run that
// registered no cancellation), which the arbiter records as
// DropReasonStopRefused instead of claiming a stop that never happened. err is
// a delivery failure. A nil StopFunc means there is no delivery side at all
// (DropReasonNoStopFunc); the composition root that owns both the Service and
// the batch supplies the real one.
type StopFunc func(ctx context.Context, scopeID, reason string) (delivered bool, err error)

// RuleArbiter is what the Service needs from the batch to route its rule
// firings through the shared arbiter instead of delivering them directly: two
// queue operations, nothing else. Keeping it narrow is what lets the Service
// stay unaware of ticks, snapshots, cooldowns and claims.
type RuleArbiter interface {
	// QueueRuleSteer queues an advisory rule's steering guidance for the
	// next tick's gauntlet.
	QueueRuleSteer(scopeID, reason, guidance string)
	// QueueRuleStop queues an advisory rule's stop of a scope.
	QueueRuleStop(scopeID, reason string)
}

// BatchConfig is the batch's tuning. Every field has a documented default, so
// the zero value is a working batch: 5s ticks, a one-tick cooldown, no cap,
// all three policy actions, and the always trigger.
type BatchConfig struct {
	// Interval is the tick cadence; <= 0 means DefaultBatchInterval.
	Interval time.Duration
	// CooldownTicks is how many ticks after an intervention the scope ignores
	// further ones. A negative value means DefaultCooldownTicks; zero
	// disables the cooldown.
	CooldownTicks int
	// MaxInterventions caps the batch's applied+claimed interventions over
	// its whole life. 0 means unlimited. A rules stop is EXEMPT in both
	// directions: the cap never blocks it and it never spends the cap — a
	// stop is the safety valve, and the design puts it above the policy
	// (design §4.6). A rules steer is an ordinary intervention: it respects
	// the cap and spends it, exactly as a policy steer does.
	MaxInterventions int
	// Actions is the policy's action mask: a decision whose action is not in
	// it is dropped (DropReasonMaskedAction). Nil means steer, redirect and
	// revert; an EMPTY (non-nil) slice masks every action, which is how a
	// configuration turns policy actions off without turning the batch off.
	Actions []supervisionapi.Action
	// Trigger selects when the caller should call its decider. "" means
	// TriggerAlways.
	Trigger Trigger
	// RootScopeID is the session's root scope: never a target, never part of
	// the batch's membership. Empty means "no root known", which leaves only
	// the membership rule to keep the root out.
	RootScopeID string
	// Now is the clock used for record timestamps; nil means time.Now.
	Now func() time.Time
	// Wait sleeps one interval, cancellable by ctx; nil means a real timer
	// selecting on ctx.Done(). Tests inject a no-op, which is what makes a
	// tick instant without a sleep.
	Wait func(ctx context.Context, d time.Duration) error
	// Stop delivers a rules stop (see StopFunc).
	Stop StopFunc
}

// normalized applies the documented defaults. Batch keeps the normalized copy,
// so a caller may keep zero-valued fields and every later read is total.
func (c BatchConfig) normalized() BatchConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultBatchInterval
	}
	if c.CooldownTicks < 0 {
		c.CooldownTicks = DefaultCooldownTicks
	}
	if c.Actions == nil {
		c.Actions = []supervisionapi.Action{
			supervisionapi.ActionSteer,
			supervisionapi.ActionRedirect,
			supervisionapi.ActionRevert,
		}
	}
	if c.Trigger == "" {
		c.Trigger = TriggerAlways
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Wait == nil {
		c.Wait = waitInterval
	}
	return c
}

// waitInterval is the default Wait: one interval, or a context cancellation
// first.
func waitInterval(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// scopeTick is the batch's per-scope bookkeeping: the last intervention the
// meta-agent (or a rule) made on the scope and how many ticks have passed
// since. It is not the scope's own activity — that is the snapshot's
// LastAction — and renderer.TickState is what it projects into.
type scopeTick struct {
	// last is the action of the last applied or claimed intervention, ""
	// when there was none.
	last supervisionapi.Action
	// age is ticks since that intervention, counted at tick boundaries (see
	// ageJustActed).
	age int
}

// pendingClaim is a redirect or revert claim that has been made but whose
// adoption the runner has not completed. It is a CLAIM, not a decision: the
// decision was applied and reported in the tick that made it, and this record
// exists only so a later rules stop can void the claim honestly (and so the
// envelope can say a successor never launched). It is pruned as soon as the
// scope's registration generation moves on — a successor registering IS the
// adoption, and a scope disappearing IS the run ending without one.
type pendingClaim struct {
	generation uint64
	action     supervisionapi.Action
	reason     string
	guidance   string
	// tick is the tick that made the claim, for the void record's provenance.
	tick int
}

// queuedRule is one rule firing waiting for the next tick's arbiter pass.
type queuedRule struct {
	stop     bool
	scopeID  string
	reason   string
	guidance string
}

// Batch is one supervision policy loop's tick engine (design §4.1): it holds
// the tick counter, the per-scope intervention ages, the label→scope map of
// the last render, the claim table and the queue of rule firings the arbiter
// shares with the rules engine.
//
// It is safe for concurrent use by one tick caller plus the Service's drain
// goroutine (the queue) and a SnapshotText renderer. Unsupported: two
// concurrent Next calls, refused with ErrBatchBusy.
type Batch struct {
	runs *Registry
	snap supervisionapi.SnapshotSource
	// cfg is normalized at construction and never written again, so it is
	// read without the lock.
	cfg BatchConfig

	// mu guards everything below. Lock ordering: Batch.mu may be taken before
	// Registry.mu (the claim prune reads a generation through it), never the
	// other way round — no registry path calls into a Batch.
	mu   sync.Mutex
	tick int // the tick the NEXT call runs; starts at 1, +1 per Next
	// inTick is the single-flight guard behind ErrBatchBusy.
	inTick bool
	state  map[string]*scopeTick
	// known and labels are the label assignment of the last SnapshotText
	// render: known is threaded back into the next render (label stability
	// across ticks), labels is what Decision.Scope resolves against.
	known  map[string]string
	labels []renderer.Labeled
	// interventions counts applied+claimed records over the batch's life,
	// the MaxInterventions budget.
	interventions int
	claims        map[string]pendingClaim
	rules         []queuedRule
	// snapshot is the last ingested snapshot and members its live, non-root
	// agents: the only scopes a decision may target.
	snapshot supervisionapi.Snapshot
	members  map[string]supervisionapi.AgentSnapshot
	// ingested records whether the first snapshot has been taken at all, so
	// the bootstrap read happens once — on whichever comes first, the first
	// render or the first tick.
	ingested bool
	// changed is the last ingest's change verdict (TriggerOnChange).
	changed bool
}

// NewBatch builds a batch. No snapshot is read here: the first one is taken
// on demand (ensureIngested) by whichever comes first — the first
// SnapshotText render or the first Next call — so constructing a batch costs
// nothing and a caller that renders before its first tick (the natural
// agent-mode loop) renders real scopes instead of an empty universe.
//
// runs is the session's run registry (nil makes every claim drop no_run);
// snap is the snapshot source (nil yields an empty snapshot, which makes the
// batch report Done immediately — the conservative failure for a caller
// waiting on it).
func NewBatch(runs *Registry, snap supervisionapi.SnapshotSource, cfg BatchConfig) *Batch {
	return &Batch{
		runs:   runs,
		snap:   snap,
		cfg:    cfg.normalized(),
		tick:   1,
		state:  make(map[string]*scopeTick),
		known:  make(map[string]string),
		claims: make(map[string]pendingClaim),
	}
}

// ensureIngested takes the batch's first snapshot, once. It is the bootstrap
// read: without it the first render would describe nothing and the first tick
// would validate every decision against an empty membership.
func (b *Batch) ensureIngested() {
	b.mu.Lock()
	done := b.ingested
	b.mu.Unlock()
	if done {
		return
	}
	fresh := b.takeSnapshot()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ingested {
		// Another caller won the race: keep its snapshot, not this one.
		return
	}
	b.ingested = true
	b.snapshot = fresh
	b.members = membersOf(fresh, b.cfg.RootScopeID)
	b.changed = changedSince(supervisionapi.Snapshot{}, b.members)
}

// Tick reports the tick number the next Next call will run. The counter starts
// at 1 and advances once per Next, whether that tick completed or its wait was
// cancelled.
func (b *Batch) Tick() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tick
}

// Changed reports whether the ingest that ended the last tick saw a change
// (see TriggerOnChange). A caller under TriggerOnChange reads it to decide
// whether to spend a model call at all.
func (b *Batch) Changed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.changed
}

// ShouldDecide reports whether the configured trigger wants the caller's
// decider invoked before the next Next call: yes under
// TriggerAlways/TriggerOnChange-and-changed, no under TriggerOnChange after a
// quiet tick — and never for a tick with no live worker, whatever the trigger
// (design §4.4: `always` means every tick that has a live agent, and the
// glossary's "a tick with no live worker is skipped" is the same rule). There
// is nothing for a decider to decide about a batch whose every worker has
// finished.
//
// The first call takes the batch's bootstrap snapshot (ensureIngested), so the
// answer before the first render is about real scopes rather than an empty
// universe.
func (b *Batch) ShouldDecide() bool {
	b.ensureIngested()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.members) == 0 {
		return false
	}
	return b.cfg.Trigger != TriggerOnChange || b.changed
}

// SnapshotText renders the LAST INGESTED snapshot as meta-agent prompt text
// (design §4.3), threading the batch's own per-scope ages and the previous
// render's labels so a worker keeps its name across ticks. It stores the
// returned labels, which is what Next resolves a Decision.Scope against.
//
// It renders the last ingested snapshot rather than polling a fresh one on
// purpose: the decision the caller's decider returns must be validated against
// the exact text it read, and a fresh poll between the render and the Next call
// would be a second, unvalidated view. A caller therefore renders once per
// tick, after the previous Next returned.
//
// budgetTokens bounds the rendered text; <= 0 selects the renderer's default.
func (b *Batch) SnapshotText(budgetTokens int) string {
	if budgetTokens <= 0 {
		budgetTokens = renderer.DefaultRenderBudgetTokens
	}
	b.ensureIngested()
	b.mu.Lock()
	defer b.mu.Unlock()
	text, labels := renderer.Render(b.snapshot, renderer.RenderOptions{
		Tick:         b.tick,
		BudgetTokens: budgetTokens,
		State:        b.renderStateLocked(),
		Known:        b.known,
	})
	b.labels = labels
	known := make(map[string]string, len(labels))
	for _, l := range labels {
		known[l.ScopeID] = l.Label
	}
	b.known = known
	return text
}

// renderStateLocked projects the batch's ages into the renderer's view. The
// rendered ticks_ago is age+1: an intervention made in tick N reads
// ticks_ago=1 when the next tick renders, which is "one tick since" as a
// reader counts it.
func (b *Batch) renderStateLocked() map[string]renderer.TickState {
	if len(b.state) == 0 {
		return nil
	}
	out := make(map[string]renderer.TickState, len(b.state))
	for scopeID, st := range b.state {
		if st.last == "" {
			continue
		}
		out[scopeID] = renderer.TickState{
			LastIntervention: string(st.last),
			TicksAgo:         st.age + 1,
		}
	}
	return out
}

// QueueRuleSteer queues an advisory rule's steering for the next tick. The
// Service's drain goroutine is the caller; one rule action per scope per tick is
// applied — the highest-ranked one, so a stop queued after a steer still wins —
// and the others are recorded as duplicates.
func (b *Batch) QueueRuleSteer(scopeID, reason, guidance string) {
	if scopeID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rules = append(b.rules, queuedRule{scopeID: scopeID, reason: reason, guidance: guidance})
}

// QueueRuleStop queues an advisory rule's stop of a scope for the next tick.
// A queued stop voids a pending policy claim on the scope — but only once it has
// actually been DELIVERED, and only when the claim's generation is still the
// live one (see applyRuleStop and voidClaim): the runner's adoption loop lets an
// existing stop win over a pending redirect, so a claim this stop really ends
// must be reported as voided, while one whose successor already launched must
// not be.
func (b *Batch) QueueRuleStop(scopeID, reason string) {
	if scopeID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rules = append(b.rules, queuedRule{stop: true, scopeID: scopeID, reason: reason})
}

// Next executes exactly one tick (design §4.1): drain the queued rule firings
// through the arbiter, validate and apply the caller's decisions against the
// last ingested snapshot, wait one interval with zero model turns, ingest a
// fresh snapshot, bump the intervention ages, evaluate the trigger, and return
// the result.
//
// The caller's convention is RENDER ONCE PER TICK — SnapshotText, then the
// decider, then Next — so "the snapshot the decider read" and "the snapshot the
// arbiter validated against" are the same text.
//
// decisions are validated against the last ingested snapshot and the last
// SnapshotText label map, never stored, so a stale decision cannot reach a
// DIFFERENT INCARNATION of a scope: the labels it names are the ones the batch
// reserved, and membership is that snapshot's. Freshness is a different promise
// and this function does not make it: a decision made from tick N's render is
// still applicable at tick N+3 when the scope stayed live and no render
// replaced the labels, because nothing in the batch ages a decision. The label
// map is what a caller refreshes, by rendering again.
//
// Decision.Scope accepts the rendered label ("w2") or a raw scope ID
// ("scope:sub-a") — deliberately, because the supervise tool's caller may name
// either; see resolveScope.
//
// A cancelled wait returns the records this tick already made plus Done as it
// stands, together with ctx's error: the interventions in the returned result
// really happened and must not be lost. The cancelled tick skips the ingest, so
// the next tick validates against the same membership — it does not skip the
// clock: the tick number advances and the intervention ages are bumped, because
// an intervention made in this tick is one tick old when the next one runs.
func (b *Batch) Next(ctx context.Context, decisions []supervisionapi.Decision) (supervisionapi.TickResult, error) {
	if err := b.beginTick(); err != nil {
		return supervisionapi.TickResult{}, err
	}
	defer b.endTick()
	b.ensureIngested()

	b.mu.Lock()
	tick := b.tick
	queued := b.rules
	b.rules = nil
	b.mu.Unlock()

	results := make([]supervisionapi.InterventionRecord, 0, len(queued)+len(decisions))
	// The rules half of the tick. One rule action per scope, chosen by ACTION
	// RANK and not by arrival order: the Service's drain queues each firing as
	// it fires, so a stop queued behind a steer on the same scope must still
	// win (design §4.6: stop > revert > redirect > steer). The losers are
	// recorded as duplicates, and rulesOn carries what the rules actually
	// accomplished on each scope for the policy half to read.
	ruleWon := ruleWinners(queued)
	rulesOn := make(map[string]rulesOutcome, len(queued))
	for i, q := range queued {
		if ruleWon[q.scopeID] != i {
			results = append(results, b.dropRecord(tick, supervisionapi.SourceRules, q.scopeID,
				q.action(), q.reason, q.guidance, supervisionapi.DropReasonDuplicate, 0))
			continue
		}
		applied := b.applyRule(ctx, q, tick)
		results = append(results, applied...)
		if o := ruleOutcomeOf(applied); o.stopped || o.steered {
			rulesOn[q.scopeID] = o
		}
	}
	results = append(results, b.applyDecisions(ctx, tick, decisions, rulesOn)...)

	if err := b.cfg.Wait(ctx, b.cfg.Interval); err != nil {
		b.mu.Lock()
		last := b.snapshot
		// The cancelled tick skips the ingest but not the clock: the tick
		// number advances below (endTick), so the ages must advance with it.
		b.bumpAgesLocked()
		done := len(b.members) == 0
		b.mu.Unlock()
		return supervisionapi.TickResult{
			Snapshot: last,
			Applied:  appliedOf(results),
			Results:  results,
			Done:     done,
		}, err
	}

	fresh := b.takeSnapshot()
	b.mu.Lock()
	prev := b.snapshot
	b.snapshot = fresh
	b.members = membersOf(fresh, b.cfg.RootScopeID)
	b.pruneClaimsLocked()
	b.bumpAgesLocked()
	b.changed = changedSince(prev, b.members)
	done := len(b.members) == 0
	b.mu.Unlock()

	// Done is "no live non-root scope is left". A batch that never saw a scope
	// is vacuously done — deliberately, because the caller waiting on it
	// (fy28's wait:settled) must not hang on a dispatch universe that never
	// had a worker in it.
	return supervisionapi.TickResult{
		Snapshot: fresh,
		Applied:  appliedOf(results),
		Results:  results,
		Done:     done,
	}, nil
}

// action is the record action a queued rule names.
func (q queuedRule) action() supervisionapi.Action {
	if q.stop {
		return supervisionapi.ActionStop
	}
	return supervisionapi.ActionSteer
}

// beginTick takes the single-flight guard.
func (b *Batch) beginTick() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inTick {
		return ErrBatchBusy
	}
	b.inTick = true
	return nil
}

// endTick releases the guard and advances the tick counter exactly once per
// Next — including a tick whose wait was cancelled, so two Next calls can never
// report records under the same tick number.
func (b *Batch) endTick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inTick = false
	b.tick++
}

// takeSnapshot reads the source. A nil source yields the empty snapshot: the
// honest "nothing is running that I can see", which makes Done true.
func (b *Batch) takeSnapshot() supervisionapi.Snapshot {
	if b.snap == nil {
		return supervisionapi.Snapshot{At: b.cfg.Now()}
	}
	return b.snap.Snapshot()
}

// membersOf is the batch's membership: the LIVE, non-root agents. A finished
// agent stays in the snapshot (its facts are the last thing a supervisor
// reads) but is not a target — the run it named is gone.
func membersOf(snap supervisionapi.Snapshot, rootScopeID string) map[string]supervisionapi.AgentSnapshot {
	out := make(map[string]supervisionapi.AgentSnapshot, len(snap.Agents))
	for _, a := range snap.Agents {
		if !a.StillRunning {
			continue
		}
		if rootScopeID != "" && a.ScopeID == rootScopeID {
			continue
		}
		out[a.ScopeID] = a
	}
	return out
}

// bumpAgesLocked advances every acted scope's cooldown age by one tick.
func (b *Batch) bumpAgesLocked() {
	for _, st := range b.state {
		if st.last == "" {
			continue
		}
		st.age++
	}
}

// pruneClaimsLocked drops claims whose scope moved on: a successor
// registration (the adoption happened) or no live run at all (the run ended
// without one). A claim that outlives both would be a claim nobody can void.
func (b *Batch) pruneClaimsLocked() {
	for scopeID, claim := range b.claims {
		gen, ok := b.generationLocked(scopeID)
		if !ok || gen != claim.generation {
			delete(b.claims, scopeID)
		}
	}
}

// generationLocked reads a scope's live registration generation. It takes the
// registry lock under Batch.mu — safe by the lock ordering documented on mu.
func (b *Batch) generationLocked(scopeID string) (uint64, bool) {
	if b.runs == nil {
		return 0, false
	}
	return b.runs.Generation(scopeID)
}

// changedSince is the TriggerOnChange verdict (design §4.4): a scope advanced
// its Through cursor, a scope's consecutive-error count grew, or two scopes in
// the batch named the same modified file. The first two are compared against
// the previous snapshot; the file overlap is a property of the current one
// alone.
//
// A scope APPEARING is not a change by this definition: a brand new worker has
// no work to react to yet, and its cursor only advances once it does
// something. That keeps a quiet dispatch quiet.
func changedSince(prev supervisionapi.Snapshot, members map[string]supervisionapi.AgentSnapshot) bool {
	prevBy := make(map[string]supervisionapi.AgentSnapshot, len(prev.Agents))
	for _, a := range prev.Agents {
		prevBy[a.ScopeID] = a
	}
	owners := make(map[string]string, 8)
	for scopeID, a := range members {
		if p, ok := prevBy[scopeID]; ok {
			if a.Through > p.Through || a.ConsecutiveErrors > p.ConsecutiveErrors {
				return true
			}
		}
		for _, f := range a.ModifiedFiles {
			if owner, ok := owners[f]; ok && owner != scopeID {
				return true
			}
			owners[f] = scopeID
		}
	}
	return false
}

// applyDecisions runs the policy half of the arbiter: resolve, then per-scope
// precedence, then the gauntlet, then the claim (design §4.6 steps 1-6). rulesOn
// is what the rules half already accomplished on each scope this tick.
func (b *Batch) applyDecisions(ctx context.Context, tick int, decisions []supervisionapi.Decision, rulesOn map[string]rulesOutcome) []supervisionapi.InterventionRecord {
	if len(decisions) == 0 {
		return nil
	}
	labels, resolved := b.resolutionTables()
	winners := winnersOf(decisions, labels, resolved, b.actionAllowed)
	out := make([]supervisionapi.InterventionRecord, 0, len(decisions))
	for i, d := range decisions {
		rec, keep := b.applyDecision(ctx, tick, decisions, labels, resolved, winners, rulesOn, i, d)
		if keep {
			out = append(out, rec)
		}
	}
	return out
}

// winnersOf picks, per scope, the decision the tick will consider: the highest
// precedence one, earliest on a tie. One decision per scope per tick is the
// decider's contract (a scope named twice is a decider bug), so the arbiter
// keeps the strongest and drops the rest as duplicates rather than applying
// two interventions to one worker.
//
// An action outside the mask never wins: the mask says the arbiter may not take
// it, and letting it hold the scope would drop an allowed lower-precedence
// decision on the strength of an action that was never executable. A masked
// decision is still recorded on its own (masked_action, not duplicate).
func winnersOf(decisions []supervisionapi.Decision, labels map[string]string, resolved map[string]bool, allowed func(supervisionapi.Action) bool) map[string]int {
	winners := make(map[string]int, len(decisions))
	for i, d := range decisions {
		if policyRank(d.Action) == 0 || !allowed(d.Action) {
			continue
		}
		scopeID, ok := resolveScope(d.Scope, labels, resolved)
		if !ok {
			continue
		}
		cur, seen := winners[scopeID]
		if !seen || policyRank(d.Action) > policyRank(decisions[cur].Action) {
			winners[scopeID] = i
		}
	}
	return winners
}

// policyRank orders the policy's actions by the design's precedence (revert >
// redirect > steer > none/unknown).
func policyRank(a supervisionapi.Action) int {
	switch a {
	case supervisionapi.ActionRevert:
		return 3
	case supervisionapi.ActionRedirect:
		return 2
	case supervisionapi.ActionSteer:
		return 1
	}
	return 0
}

// ruleWinners picks, per scope, the queued rule action this tick applies: the
// highest-precedence one, earliest on a tie. The queue's own order is arrival
// order — the Service's drain emits one entry per firing — and arrival order is
// not precedence, so a stop queued behind a steer on the same scope still wins.
// One rule action per scope per tick, exactly as one decision per scope per tick.
func ruleWinners(queued []queuedRule) map[string]int {
	winners := make(map[string]int, len(queued))
	for i, q := range queued {
		cur, seen := winners[q.scopeID]
		if !seen || ruleRank(q) > ruleRank(queued[cur]) {
			winners[q.scopeID] = i
		}
	}
	return winners
}

// ruleRank is the rules half's precedence (design §4.6). The numbers sit on the
// same scale policyRank uses, with the policy-only slots skipped: a stop above
// everything the policy can do, a steer below everything.
func ruleRank(q queuedRule) int {
	if q.stop {
		return 4
	}
	return 1
}

// rulesOutcome is what the rules half of the tick accomplished on one scope, as
// the policy half reads it (design §4.6 precedence, applied BETWEEN the halves).
//
// The bar is deliberately high, because a rule that accomplished nothing must
// not silence the model's decision: only a stop that was actually DELIVERED, and
// a steer the tick actually attempted, count as the rules having acted.
type rulesOutcome struct {
	// stopped reports a rules stop delivered on the scope this tick. A stop
	// outranks every policy action.
	stopped bool
	// steered reports a rules steer the tick ATTEMPTED on the scope — applied,
	// or dropped on the cooldown, which is the pinned suppression: the rules
	// engine's steer and the policy's are the same intervention, so cooldown
	// having eaten the first does not earn the second a try. A rules steer
	// outranks only a policy steer; a policy revert or redirect on the same
	// scope still goes ahead.
	steered bool
}

// ruleOutcomeOf reads what a scope's rules pass accomplished from the records
// the arbiter produced — the records are the report, so the precedence decision
// and the operator read the same account. A rule dropped for any other reason
// (no stop delivery function, a refused or failed stop, empty guidance, no live
// run, the cap) leaves the scope to the policy's gauntlet.
func ruleOutcomeOf(recs []supervisionapi.InterventionRecord) rulesOutcome {
	var out rulesOutcome
	for _, r := range recs {
		if r.Source != supervisionapi.SourceRules {
			// A stop's void record is a policy-sourced fact about an earlier
			// tick's claim, not an action this tick.
			continue
		}
		switch {
		case r.Action == supervisionapi.ActionStop && r.Outcome == supervisionapi.OutcomeApplied:
			out.stopped = true
		case r.Action == supervisionapi.ActionSteer &&
			(r.Outcome == supervisionapi.OutcomeApplied ||
				(r.Outcome == supervisionapi.OutcomeDropped && r.DropReason == supervisionapi.DropReasonCooldown)):
			out.steered = true
		}
	}
	return out
}

// resolutionTables reads the label map of the last render and the set of scope
// IDs the last snapshot knows about (live or finished).
func (b *Batch) resolutionTables() (map[string]string, map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels := make(map[string]string, len(b.labels))
	for _, l := range b.labels {
		labels[l.Label] = l.ScopeID
	}
	known := make(map[string]bool, len(b.snapshot.Agents))
	for _, a := range b.snapshot.Agents {
		known[a.ScopeID] = true
	}
	return labels, known
}

// resolveScope maps a Decision.Scope onto a scope ID. BOTH forms are accepted:
// the rendered label ("w2") and the bare scope ID ("scope:sub-a"), because the
// supervise tool's caller may name either, and a label always wins when a
// string is somehow both. An unresolvable name is not a scope at all — it is a
// label from a render the batch no longer remembers.
func resolveScope(name string, labels map[string]string, known map[string]bool) (string, bool) {
	if id, ok := labels[name]; ok {
		return id, true
	}
	if known[name] {
		return name, true
	}
	return "", false
}

// applyDecision runs one policy decision through the whole gauntlet and returns
// its record, or keep=false for the one silent case (action none, which is what
// an unlisted scope means and is not an outcome worth a record).
func (b *Batch) applyDecision(ctx context.Context, tick int, decisions []supervisionapi.Decision, labels map[string]string, known map[string]bool, winners map[string]int, rulesOn map[string]rulesOutcome, i int, d supervisionapi.Decision) (supervisionapi.InterventionRecord, bool) {
	source := supervisionapi.SourcePolicy

	// "none" is the absence of a decision, not a dropped one.
	if d.Action == supervisionapi.ActionNone {
		return supervisionapi.InterventionRecord{}, false
	}
	// Target validation, step 1. The vocabulary check comes first: an action
	// the arbiter cannot execute says nothing about any scope.
	if policyRank(d.Action) == 0 {
		return b.dropRecord(tick, source, d.Scope, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonUnknownAction, 0), true
	}
	// The root scope first: it is a live registration, so the membership check
	// below would otherwise report it as merely "not live" and leave
	// root_scope unreachable.
	if b.cfg.RootScopeID != "" && d.Scope == b.cfg.RootScopeID {
		return b.dropRecord(tick, source, d.Scope, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonRootScope, 0), true
	}
	scopeID, ok := resolveScope(d.Scope, labels, known)
	if !ok {
		return b.dropRecord(tick, source, d.Scope, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonStale, 0), true
	}
	if b.cfg.RootScopeID != "" && scopeID == b.cfg.RootScopeID {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonRootScope, 0), true
	}
	member, live := b.member(scopeID)
	if !live {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonNotLive, 0), true
	}
	if !b.actionAllowed(d.Action) {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonMaskedAction, 0), true
	}
	// Precedence between the tick's two halves, then inside the policy's. A
	// delivered rules stop outranks everything on the scope; a rules steer
	// outranks only a policy steer, so the policy's stronger actions still
	// reach their gauntlet (see rulesOutcome).
	if r := rulesOn[scopeID]; r.stopped || (r.steered && d.Action == supervisionapi.ActionSteer) {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonSuperseded, 0), true
	}
	if w, seen := winners[scopeID]; seen && w != i {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonDuplicate, 0), true
	}
	// Step 2: cooldown.
	if b.coolingDown(scopeID) {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonCooldown, 0), true
	}
	// Step 3: preconditions.
	guidance := truncateWords(d.Guidance, supervisionapi.MaxGuidanceWords)
	if guidance == "" {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonEmptyGuidance, 0), true
	}
	if d.Action == supervisionapi.ActionRevert && !member.Reversible {
		// Never downgraded to a redirect: the decider asked to undo the run's
		// damage, and a redirect would keep it.
		gen, _ := b.generation(scopeID)
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonNotReversible, gen), true
	}
	// Step 4: cap.
	if b.capReached() {
		return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.DropReasonCapReached, 0), true
	}

	// Step 5: claim. Every delivery call happens without the batch lock held:
	// Inject, Redirect and CancelGeneration take the registry's lock, and a
	// StopFunc may block for a bounded quiesce wait.
	switch d.Action {
	case supervisionapi.ActionSteer:
		if err := b.inject(scopeID, guidance); err != nil {
			return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
				steerDropReason(err), 0), true
		}
		// TOCTOU, accepted and documented: Inject is SCOPE-keyed, not
		// generation-keyed. A steer validated against generation G can land in
		// G+1's steering buffer when a handoff adopted between the validation
		// and the send. The guidance is still about this scope and still
		// useful — the successor is working the same task — so this is a
		// bounded, honest imprecision rather than a correctness hole.
		b.noteApplied(scopeID, d.Action)
		return b.newRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.OutcomeApplied, "", 0), true
	default: // redirect, revert
		gen, ok := b.generation(scopeID)
		if !ok {
			return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
				supervisionapi.DropReasonNoRun, 0), true
		}
		handle, err := b.redirect(scopeID, gen, supervisionapi.RedirectRequest{
			Guidance: guidance,
			Revert:   d.Action == supervisionapi.ActionRevert,
		})
		if err != nil {
			reason := supervisionapi.DropReasonNoRun
			if errors.Is(err, supervisionapi.ErrAlreadyClaimed) {
				reason = supervisionapi.DropReasonAlreadyClaimed
			}
			return b.dropRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance, reason, gen), true
		}
		// The claim is recorded BEFORE the stop is requested: the runner's
		// adoption loop is asynchronous, and a rules stop on a later tick has
		// to be able to void it.
		b.addClaim(scopeID, pendingClaim{
			generation: handle.Gen,
			action:     d.Action,
			reason:     truncateWords(d.Reason, supervisionapi.MaxReasonWords),
			guidance:   guidance,
			tick:       tick,
		})
		// The stop is what lets the runner's adoption loop start the successor.
		// ErrNoRun here is NORMAL, not a failure: the run finished between the
		// claim and the cancel, which is exactly the state the adoption loop
		// wants. Anything else (no cancellation registered) does not fail the
		// record either — the claim is real, and the runner owns the rest.
		if _, cerr := b.cancelGeneration(scopeID, handle.Gen); cerr != nil && !cancelNotFatal(cerr) {
			slog.Warn("supervision: redirect claimed but the run could not be stopped; the adoption loop owns the outcome",
				"scope", scopeID, "generation", handle.Gen, "err", cerr)
		}
		b.noteApplied(scopeID, d.Action)
		// TOCTOU, accepted and documented: a claimed redirect may still be
		// voided by a supervisor stop in the runner's adoption loop — the
		// runner checks haltedBySupervisor before launching a successor. The
		// record therefore says "claimed", never "redirected": only the
		// adoption can say a successor exists.
		return b.newRecord(tick, source, scopeID, d.Action, d.Reason, d.Guidance,
			supervisionapi.OutcomeClaimed, "", handle.Gen), true
	}
}

// applyRule runs one queued rule firing through the same arbiter. A rule
// addresses a scope ID it read from the trace, so there is no label to resolve;
// the mask is not applied either (supervisor.actions governs the POLICY's
// actions, and the rules engine has its own thresholds), but the cooldown and
// the cap are, because a rule intervention costs a worker's context the same
// way a policy one does.
func (b *Batch) applyRule(ctx context.Context, q queuedRule, tick int) []supervisionapi.InterventionRecord {
	source := supervisionapi.SourceRules
	if q.stop {
		return b.applyRuleStop(ctx, q, tick)
	}
	if b.cfg.RootScopeID != "" && q.scopeID == b.cfg.RootScopeID {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.DropReasonRootScope, 0)}
	}
	if _, live := b.member(q.scopeID); !live {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.DropReasonNotLive, 0)}
	}
	if b.coolingDown(q.scopeID) {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.DropReasonCooldown, 0)}
	}
	guidance := truncateWords(q.guidance, supervisionapi.MaxGuidanceWords)
	if guidance == "" {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.DropReasonEmptyGuidance, 0)}
	}
	if b.capReached() {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.DropReasonCapReached, 0)}
	}
	if err := b.inject(q.scopeID, guidance); err != nil {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionSteer, q.reason, q.guidance, steerDropReason(err), 0)}
	}
	b.noteApplied(q.scopeID, supervisionapi.ActionSteer)
	return []supervisionapi.InterventionRecord{b.newRecord(tick, source, q.scopeID,
		supervisionapi.ActionSteer, q.reason, q.guidance, supervisionapi.OutcomeApplied, "", 0)}
}

// applyRuleStop delivers a rules stop and, when it was actually DELIVERED,
// voids the scope's pending policy claim. The stop record comes first (rules
// records lead their tick), then the voided claim's: that claim was reported as
// "claimed" in the tick that made it and will never reach adoption, so the
// history must say so.
//
// The order inside is deliberate — deliver first, then void — because the record
// follows the effect and not the intent. A refusal (the halt_root gate, a merged
// scope, a run with no cancellation) or a delivery failure leaves the claim in
// the table, where the runner will still adopt it: voiding a claim nobody
// stopped would report a successor that never launched.
//
// A stop is exempt from the cap in both directions (never blocked, never
// spending) and is not gated on membership — the delivery function reports
// honestly about a scope with no run, and the halt_root gate that decides
// whether a root stop may be queued at all lives in the Service.
func (b *Batch) applyRuleStop(ctx context.Context, q queuedRule, tick int) []supervisionapi.InterventionRecord {
	source := supervisionapi.SourceRules
	if b.cfg.Stop == nil {
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionStop, q.reason, "", supervisionapi.DropReasonNoStopFunc, 0)}
	}
	delivered, err := b.cfg.Stop(ctx, q.scopeID, q.reason)
	if err != nil {
		slog.Warn("supervision: rule stop delivery failed", "scope", q.scopeID, "err", err)
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionStop, q.reason, "", supervisionapi.DropReasonStopFailed, 0)}
	}
	if !delivered {
		// A refusal, not a failure: the delivery side decided this scope may
		// not be stopped. Nothing happened, so nothing is claimed stopped and
		// no claim is voided.
		return []supervisionapi.InterventionRecord{b.dropRecord(tick, source, q.scopeID,
			supervisionapi.ActionStop, q.reason, "", supervisionapi.DropReasonStopRefused, 0)}
	}
	b.noteApplied(q.scopeID, supervisionapi.ActionStop)
	out := []supervisionapi.InterventionRecord{b.newRecord(tick, source, q.scopeID,
		supervisionapi.ActionStop, q.reason, "", supervisionapi.OutcomeApplied, "", 0)}
	if void, ok := b.voidClaim(q.scopeID, tick); ok {
		out = append(out, void)
	}
	return out
}

// voidClaim removes the scope's pending policy claim and reports the record for
// it — but only when the claim is still about the incarnation the stop ended. A
// claim made in tick N can outlive its run: the runner may have adopted it
// (registering a successor generation) or the run may have finished before this
// tick's stop landed, and voiding it then would report a false supersede beside
// a stop that halted the SUCCESSOR. So the claim's generation is compared with
// the live registration's before any record is made; a claim that no longer
// matches is pruned silently, which is what the tick-end prune
// (pruneClaimsLocked) would have done anyway.
func (b *Batch) voidClaim(scopeID string, tick int) (supervisionapi.InterventionRecord, bool) {
	b.mu.Lock()
	claim, ok := b.claims[scopeID]
	if ok {
		delete(b.claims, scopeID)
	}
	// The generation read happens under Batch.mu, the documented lock order.
	gen, live := b.generationLocked(scopeID)
	b.mu.Unlock()
	if !ok || !live || gen != claim.generation {
		return supervisionapi.InterventionRecord{}, false
	}
	return b.dropRecord(tick, supervisionapi.SourcePolicy, scopeID, claim.action,
		claim.reason, claim.guidance, supervisionapi.DropReasonSupersededByStop, claim.generation), true
}

// cancelNotFatal reports whether a CancelGeneration failure leaves the claim
// record intact. ErrNoRun is the NORMAL race — the run finished between the
// claim and the cancel, which is exactly the state the adoption loop wants —
// and ErrNotHaltable means the run registered no cancellation, which the runner
// owns. The claim is real either way: it is a fact about the registration, not
// about whether the stop landed, and only the adoption loop can say a successor
// exists.
func cancelNotFatal(err error) bool {
	return errors.Is(err, supervisionapi.ErrNoRun) || errors.Is(err, supervisionapi.ErrNotHaltable)
}

// steerDropReason classifies an Inject failure. Anything the registry did not
// classify is reported as no_run: the guidance did not arrive, and "no run to
// steer" is the honest summary of that.
func steerDropReason(err error) string {
	if errors.Is(err, supervisionapi.ErrSteerBusy) {
		return supervisionapi.DropReasonSteerBusy
	}
	return supervisionapi.DropReasonNoRun
}

// inject delivers guidance, tolerating a nil registry (every claim drops).
func (b *Batch) inject(scopeID, guidance string) error {
	if b.runs == nil {
		return supervisionapi.ErrNoRun
	}
	return b.runs.Inject(scopeID, guidance)
}

// redirect claims a handoff, tolerating a nil registry.
func (b *Batch) redirect(scopeID string, gen uint64, req supervisionapi.RedirectRequest) (RedirectHandle, error) {
	if b.runs == nil {
		return RedirectHandle{}, supervisionapi.ErrNoRun
	}
	return b.runs.Redirect(scopeID, gen, req)
}

// generation reads the live registration generation, tolerating a nil registry.
func (b *Batch) generation(scopeID string) (uint64, bool) {
	if b.runs == nil {
		return 0, false
	}
	return b.runs.Generation(scopeID)
}

// cancelGeneration asks the runner to stop the claimed generation.
func (b *Batch) cancelGeneration(scopeID string, gen uint64) (<-chan struct{}, error) {
	if b.runs == nil {
		return nil, supervisionapi.ErrNoRun
	}
	return b.runs.CancelGeneration(scopeID, gen)
}

// member reports the scope's membership entry from the last ingest.
func (b *Batch) member(scopeID string) (supervisionapi.AgentSnapshot, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[scopeID]
	return m, ok
}

// coolingDown reports whether the scope is inside its cooldown window.
func (b *Batch) coolingDown(scopeID string) bool {
	if b.cfg.CooldownTicks <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.state[scopeID]
	if !ok || st.last == "" {
		return false
	}
	return st.age < b.cfg.CooldownTicks
}

// capReached reports whether the batch's intervention budget is spent.
func (b *Batch) capReached() bool {
	if b.cfg.MaxInterventions <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.interventions >= b.cfg.MaxInterventions
}

// actionAllowed reports whether the action is inside the configured mask. cfg is
// immutable after construction, so this needs no lock.
func (b *Batch) actionAllowed(a supervisionapi.Action) bool {
	for _, m := range b.cfg.Actions {
		if m == a {
			return true
		}
	}
	return false
}

// noteApplied records an intervention: the scope's age resets (it was acted on
// this tick) and the batch's budget is spent by one — except for a stop, which
// is exempt in both directions: the cap never blocks it and it never spends the
// cap (see BatchConfig.MaxInterventions).
func (b *Batch) noteApplied(scopeID string, action supervisionapi.Action) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state[scopeID] = &scopeTick{last: action, age: ageJustActed}
	if action == supervisionapi.ActionStop {
		return
	}
	b.interventions++
}

// addClaim registers a claim awaiting adoption.
func (b *Batch) addClaim(scopeID string, c pendingClaim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claims[scopeID] = c
}

// newRecord builds a record whose model-authored text is already inside the
// decision's word limits.
func (b *Batch) newRecord(tick int, source, scopeID string, action supervisionapi.Action, reason, guidance, outcome, dropReason string, gen uint64) supervisionapi.InterventionRecord {
	return supervisionapi.InterventionRecord{
		At:         b.cfg.Now(),
		Tick:       tick,
		Scope:      scopeID,
		Source:     source,
		Action:     action,
		Reason:     truncateWords(reason, supervisionapi.MaxReasonWords),
		Guidance:   truncateWords(guidance, supervisionapi.MaxGuidanceWords),
		Outcome:    outcome,
		DropReason: dropReason,
		Generation: gen,
	}
}

// dropRecord builds a dropped record. It is the arbiter's single exit for a
// decision that took no effect, which is what keeps every drop named.
func (b *Batch) dropRecord(tick int, source, scopeID string, action supervisionapi.Action, reason, guidance, dropReason string, gen uint64) supervisionapi.InterventionRecord {
	return b.newRecord(tick, source, scopeID, action, reason, guidance,
		supervisionapi.OutcomeDropped, dropReason, gen)
}

// appliedOf filters a tick's results down to the interventions that took
// effect, preserving order (TickResult.Applied is a subset of Results in the
// same order).
func appliedOf(results []supervisionapi.InterventionRecord) []supervisionapi.InterventionRecord {
	var out []supervisionapi.InterventionRecord
	for _, r := range results {
		if r.Outcome == supervisionapi.OutcomeApplied || r.Outcome == supervisionapi.OutcomeClaimed {
			out = append(out, r)
		}
	}
	return out
}

// truncateWords keeps the first max whitespace-separated words and drops the
// rest, so a truncation never cuts a word in half. It counts exactly as
// supervisionapi.WordCount does, which is the same rule the headless decider
// (yaah-0nbb) enforces on its own replies.
func truncateWords(s string, max int) string {
	if max <= 0 {
		return ""
	}
	fields := strings.Fields(s)
	if len(fields) <= max {
		return s
	}
	return strings.Join(fields[:max], " ")
}
