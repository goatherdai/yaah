// Package supervision bridges goatherd scopes to the loops that run
// under them (goatherd-adoption plan D2): a scope's interventions must
// reach execution, not just the trace. The Registry maps scope IDs to
// the running loops' steering channels, so the supervisor tool's inject
// (and, from step A3 on, halt) addresses something real.
//
// The registry is deliberately dumb: it knows scope IDs and channels,
// nothing about KEEPgoatherdedKEEP scopes or loop internals — the runner
// registers, the supervisor tool delivers, and lifecycle (a run's
// deferred unregister) keeps the map honest.
package supervision

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// ErrNoRun reports an Inject at a scope with no registered run — the
// scope may exist in the trace manager while its loop has finished (or
// never registered). Callers fall back to trace-only recording.
var ErrNoRun = supervisionapi.ErrNoRun

// ErrSteerBusy reports an Inject whose target loop's steering buffer is
// full: the run is not consuming guidance (wedged between turns is the
// usual cause), and blocking would stall the supervisor for a
// wedged-run timeout.
var ErrSteerBusy = supervisionapi.ErrSteerBusy

// ErrNotHaltable reports a Cancel at a registered run that carries no
// cancellation function — injectable but not haltable. Callers report
// the honest outcome instead of claiming a halt.
var ErrNotHaltable = supervisionapi.ErrNotHaltable

// ErrAlreadyClaimed reports a Redirect whose scope and generation are
// live but already claimed by an earlier Redirect for that same
// generation. It is deliberately distinct from ErrNoRun: ErrNoRun
// means the generation no longer names a live run (unregistered or
// superseded by a successor Register), while ErrAlreadyClaimed means
// this caller arrived second at a run that is still live — and must
// not launch a second replacement loop for it.
var ErrAlreadyClaimed = supervisionapi.ErrAlreadyClaimed

// ErrAlreadyClaimed = errors.New("the run is already claimed for redirect")

// SteerBuffer is the per-run steering channel capacity, exported for the
// composition sites that create the channels the registry borrows. Steering
// is between-turn delivery: a healthy run drains this buffer every step;
// a full buffer means the run is wedged between turns.
const SteerBuffer = 8

// Run is one registered execution's bridge surface: the steering channel
// (the inject bridge, adoption plan A2), the cancellation function (the
// halt bridge, A3), and the quiesce signal — a channel the run closes
// when its loop has returned and nothing is executing inside the
// scope's sandbox anymore. The quiesce channel is what makes halt's
// teardown ordering sound: destroying an owned sandbox while an
// in-flight tool still uses it is the hazard the jobs manager's
// CloseAndWait documents. Any field may be nil — a run registers what
// it supports, and Inject or Cancel report honestly about the rest.
type Run struct {
	SteerCh chan string
	Cancel  context.CancelFunc
	Done    chan struct{}

	// Handoff ends the run WITHOUT halt semantics — the live redirect's
	// handoff signal (PR #398 review finding: a redirect's stop must not
	// read as a supervisor halt in the runner's accounting, or the
	// adoption loop's halt handling erases real halts). CancelGeneration
	// stops a run through this function; halt keeps using Cancel. Nil
	// falls back to Cancel, at the cost of that conflation.
	Handoff context.CancelFunc

	// Role, Label, Workdir, and StartedAt are the run's snapshot metadata
	// (yaah-1050.1.1): who registered, under what role, in which
	// workspace, and since when. They are advisory bookkeeping only —
	// nothing in the bridge reads them — and every one of them is
	// optional: a registrant that supplies none (the existing tests, the
	// session root) reports zero values and snapshots fine.
	Role      string
	Label     string
	Workdir   string
	StartedAt time.Time

	// Feature is the run's snapshot feature description (yaah-k0x1): the
	// dispatch label when one was supplied, otherwise the dispatch prompt
	// collapsed and clipped — derived by the registrant with DeriveFeature
	// BEFORE Register, so the registry never holds a full prompt.
	Feature string

	// Redirectable says a live-redirect handoff may target this run. The
	// sub-agent runner sets it at Register; the root scope's registration
	// leaves it false, which is what keeps a redirect from ever replacing
	// the session's own loop.
	Redirectable bool
}

// Registration is one run as the snapshot sees it: the scope it registered
// under, its metadata, and its lifecycle clock. It deliberately does not
// carry the run's channels — a snapshot is a read of state, never a handle
// on the execution — so it stays copyable and cannot be used to steer.
type Registration struct {
	ScopeID string
	// Generation is the incarnation this registration describes: the
	// registry-wide monotonic number Register stamped on the run. The
	// snapshot reads an entry's metadata and its in-flight calls in two
	// steps, and a redirect may register a successor between them —
	// carrying the generation makes the second read generation-checked,
	// so a replaced run's entry reports no in-flight calls rather than
	// its successor's (InFlightCalls applies the same guard as
	// InFlightTable).
	Generation   uint64
	Role         string
	Label        string
	Feature      string
	Workdir      string
	StartedAt    time.Time
	FinishedAt   time.Time
	StillRunning bool
	// Reversible says a pre-run workspace capture exists, so a revert can
	// roll the scope's workspace back to it. Deliberately NOT a
	// registration input: the capture runs after Register, so the
	// registrant marks it through MarkReversible once the capture has
	// succeeded — a generation-checked write that cannot reach a
	// successor. Registration only projects what MarkReversible wrote.
	Reversible   bool
	Redirectable bool
}

// MarkReversible marks the scope's live run reversible when its
// generation still equals gen — the runner's post-capture call, recording
// that a pre-run workspace snapshot exists and a revert can restore it.
// The generation check is the stale-caller guard: a capture that finishes
// after a successor registered must not mark the replacement. Reports
// whether the mark landed.
func (r *Registry) MarkReversible(scopeID string, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return false
	}
	run.reversible = true
	return true
}

// InFlightTable returns the live run's in-flight call table when its
// generation still equals gen, and nil otherwise. The table is the feed
// seam (yaah-k0x1): the runner forwards its loop's tool start/end events
// into it, and the snapshot builder reads Entries through InFlightCalls.
// A dropped table — unregister, or a successor Register — stays writable
// but unread, so a late forwarder cannot corrupt a successor's view.
func (r *Registry) InFlightTable(scopeID string, gen uint64) *InFlightTable {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return nil
	}
	return run.inflight
}

// InFlightCalls returns the scope's live run's in-flight calls, oldest
// first, aged at now — but only when the live run's generation still
// equals gen, the same guard InFlightTable applies. Nil when no run is
// live for the scope (a finished run has no in-flight calls, which is
// what makes a call cut off by cancellation disappear from the snapshot),
// and nil when the generation was replaced: a snapshot reads an entry's
// metadata and its calls in two steps, and a redirect may register a
// successor between them — the predecessor's entry must report no calls,
// never its successor's.
func (r *Registry) InFlightCalls(scopeID string, gen uint64, now time.Time) []supervisionapi.InFlightCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return nil
	}
	return run.inflight.Entries(now)
}

// RedirectHandle is a successful Redirect claim: the scope and the
// generation the claimer verified under the lock. It is inert data —
// holding it steers, cancels, and unregisters nothing — but it is the
// token the live-redirect handoff carries when it swaps loops: the
// registration still bears this generation until a successor Register
// supersedes it, so the handle records exactly which incarnation the
// claimer won. It aliases the supervisionapi type so the supervisor
// tool can name it without importing this agent-family package.
type RedirectHandle = supervisionapi.RedirectHandle

// RedirectRequest is a claimed live-redirect handoff's payload: the
// guidance the supervisor deposited for the successor loop. The claim
// itself is the registration's redirect-pending mark (set by Redirect,
// single winner via ErrAlreadyClaimed); the request is what the parent
// loop reads through ConsumeRedirect after the current run returns and
// clears when it adopts the handoff. It aliases the supervisionapi type
// for the same boundary reason as RedirectHandle.
type RedirectRequest = supervisionapi.RedirectRequest

// maxFinishedRuns bounds the recently-finished list. A finished run is kept
// so a supervisor polling just after a dispatch ends still sees what that
// agent did (the live entry is gone the moment the loop returns); the oldest
// is dropped past the cap, so the list can never outgrow a session.
const maxFinishedRuns = 32

// run is the registry's bookkeeping for one registered execution.
type run struct {
	entry Run
	// reversible is the per-run flag behind Registration.Reversible,
	// written only by MarkReversible: no registration input sets it, so
	// the only write path is the generation-checked post-capture mark.
	reversible bool
	// gen is this incarnation's generation: a registry-wide monotonic
	// number stamped by Register. Because a later Register for the same
	// scope replaces the whole run value with a higher generation,
	// UnregisterGeneration and Redirect can compare-and-swap against a
	// specific loop incarnation rather than the bare scope name — the
	// generation-swap model that keeps an old loop's teardown or claim
	// from touching a successor.
	gen uint64
	// returned marks a generation whose loop has returned: Inject stops
	// claiming delivery while the redirect machinery keeps working —
	// the adoption window is exactly "returned but not yet consumed or
	// closed". closed marks a generation past its final adoption check:
	// no redirect may claim it anymore. Both flags live and die with the
	// generation (a later Register replaces the whole value).
	returned bool
	closed   bool
	// redirectPending records a successful Redirect claim for this
	// generation. It lives and dies with the registration, so exactly
	// one claimer wins per incarnation: the next Redirect at the same
	// generation reports ErrAlreadyClaimed.
	redirectPending bool
	// pendingRedirect carries the claimed handoff's guidance until the
	// parent loop consumes it (nil when unclaimed, or once consumed).
	pendingRedirect *RedirectRequest
	// inflight is this generation's table of started-not-returned tool
	// calls (yaah-k0x1), created at Register, cleared when the run
	// returns (MarkReturned — nothing is executing across the adoption
	// window), and dropped with the whole run value at unregister or
	// successor swap. A table a late forwarder still holds after that is
	// inert: nobody reads it, and a stale caller cannot reach a
	// successor's table — the generation check on InFlightTable sees to
	// that.
	inflight *InFlightTable
}

// Registry maps scope IDs to the loops currently running under those
// scopes.
type Registry struct {
	mu   sync.Mutex
	runs map[string]*run
	// nextGen stamps every successful Register with a strictly larger
	// generation, registry-wide — so generations for any one scope only
	// ever increase, which is what makes the generation-swap model's
	// compare-and-swap total order per scope.
	nextGen uint64
	// finished is the bounded recent-history tail, oldest first. It is
	// what keeps a snapshot useful across the exact instant a dispatch
	// ends: the entry leaves runs (so Inject and Has stop claiming a live
	// bridge) while the metadata stays readable here.
	finished []Registration
	// records is the session's intervention log (yaah-vaxv): every decision
	// and every rules action, per scope, drained by the run it names at
	// settle. It is a value so a zero Registry still records safely, and it
	// carries its own mutex — nothing here takes r.mu to append.
	records interventionLog
	// now stamps FinishedAt. A field, not a direct time.Now call, so a
	// test can pin the clock without a sleep.
	now func() time.Time
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{runs: make(map[string]*run), now: time.Now}
}

// Register associates a scope with its run: the steering channel
// (inject) and the cancellation function (halt, adoption plan A3). The
// channel is owned by the run — the registry only borrows it, and
// Unregister must be called before the run closes it. A nil channel
// registers nothing on the inject side: there is no loop side to
// deliver to, and a fabricated channel nobody drains would have Inject
// claim delivery into a void while status listed the scope as bridged.
// A nil cancel keeps the run injectable but not haltable (Cancel then
// reports ErrNotHaltable).
func (r *Registry) Register(scopeID string, entry Run) {
	if entry.SteerCh == nil {
		return
	}
	if entry.Done != nil {
		select {
		case <-entry.Done:
			// Already finished before registration — nothing to bridge.
			return
		default:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetFinishedLocked(scopeID)
	r.nextGen++
	r.runs[scopeID] = &run{entry: entry, gen: r.nextGen, inflight: newInFlightTable(r.now)}
}

// Unregister removes a run. Safe to call twice; the run's deferred
// unregister racing a session-close unregister is the normal case. The
// run's metadata moves into the bounded recently-finished list, so a
// snapshot taken just after the dispatch ends still names what the agent
// did — but the entry is no longer live: Has, Inject, and Cancel report
// ErrNoRun for it exactly as before.
func (r *Registry) Unregister(scopeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok {
		return
	}
	r.unregisterLocked(scopeID, entry)
}

// unregisterLocked moves a live entry to the bounded finished tail.
// Callers hold r.mu and have already resolved the entry, so Unregister
// and UnregisterGeneration share one removal path.
func (r *Registry) unregisterLocked(scopeID string, entry *run) {
	delete(r.runs, scopeID)
	reg := registrationOf(scopeID, entry.entry, entry.reversible, entry.gen)
	reg.StillRunning = false
	reg.FinishedAt = r.clock()
	r.finished = append(r.finished, reg)
	if over := len(r.finished) - maxFinishedRuns; over > 0 {
		r.finished = r.finished[over:]
	}
}

// Generation reports the generation of the scope's live registration —
// the token a later Redirect or UnregisterGeneration compares against.
// It reports false for an unregistered scope: generations identify live
// incarnations only, so a finished run has none.
//
// The generation-swap model: every Register stamps a new, strictly
// increasing generation on the scope's entry, so a successor loop for
// the same scope is distinguishable from its predecessor by a number
// alone. A supervisor inspects a scope, reads its Generation, and later
// redirects or the old loop unregisters against that number — never
// against the bare scope name, which a successor may already own.
func (r *Registry) Generation(scopeID string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok {
		return 0, false
	}
	return entry.gen, true
}

// UnregisterGeneration removes the registration only when its
// generation still equals gen — the run-end half of the generation-swap
// model. A loop's deferred teardown passes the generation it registered
// with; if a successor Register has already swapped the scope to a
// newer incarnation, gen no longer matches and the call is a no-op
// returning false, leaving the successor registered. A matching
// generation unregisters exactly like Unregister — metadata moves to
// the bounded finished tail — and returns true. An already-unregistered
// scope also returns false. Unregister keeps its unconditional behavior
// for existing callers.
func (r *Registry) UnregisterGeneration(scopeID string, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok || entry.gen != gen {
		return false
	}
	r.unregisterLocked(scopeID, entry)
	return true
}

// Redirect claims the scope's current run for a live-redirect handoff
// as a compare-and-swap on the generation. Under the registry lock it
// verifies the scope is registered and its generation still equals
// fromGen — the incarnation the caller inspected — then marks that
// registration redirect-pending and returns a RedirectHandle naming the
// claim. ErrNoRun reports an unregistered scope or a generation
// mismatch (the run finished, or a successor Register already swapped
// the scope): the caller must re-inspect instead of launching a
// replacement for a loop that is gone.
//
// Exactly one claimer wins per generation: a second Redirect at the
// same scope and generation finds the pending mark and reports
// ErrAlreadyClaimed — never ErrNoRun, since the run is still live, it
// is simply already spoken for — so two supervisors racing the same
// handoff cannot both launch replacements. The pending mark lives and
// dies with the registration: a new Register is a fresh, unclaimed
// generation, and Unregister ends the incarnation entirely.
func (r *Registry) Redirect(scopeID string, fromGen uint64, req RedirectRequest) (RedirectHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != fromGen {
		return RedirectHandle{}, ErrNoRun
	}
	if run.closed {
		// The generation passed its final adoption check: a claim here
		// would be reported as a fresh loop and then silently dropped at
		// teardown (PR #398 review finding). Refuse it honestly.
		return RedirectHandle{}, ErrNoRun
	}
	if run.redirectPending {
		return RedirectHandle{}, ErrAlreadyClaimed
	}
	run.redirectPending = true
	// The claim and the payload publish under ONE lock: an empty pending
	// request the parent loop could consume before the claimer deposited
	// the guidance lost the handoff entirely (PR #398 review finding). A
	// pending request is therefore born complete.
	run.pendingRedirect = &req
	return RedirectHandle{ScopeID: scopeID, Gen: fromGen}, nil
}

// ConsumeRedirectOrClose is the parent loop's post-run read: it consumes
// the generation's pending redirect if one arrived, and otherwise closes
// the generation to further redirects in the same critical section. The
// claim check and the teardown gap must not be separate — a redirect
// landing after a final ConsumeRedirect(false) used to succeed, promise a
// successor, and be silently dropped at UnregisterGeneration (PR #398
// review finding). A returned request is always complete (Redirect
// publishes the payload atomically with the claim).
func (r *Registry) ConsumeRedirectOrClose(scopeID string, gen uint64) (RedirectRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return RedirectRequest{}, false
	}
	if run.redirectPending && run.pendingRedirect != nil {
		req := *run.pendingRedirect
		run.pendingRedirect = nil
		// redirectPending stays set: the single-winner mark outlives
		// the payload slot, so a second claim on the generation is
		// refused even after the parent loop adopted the handoff.
		return req, true
	}
	run.closed = true
	return RedirectRequest{}, false
}

// registrationOf projects a run's bridge surface down to its snapshot
// metadata: the channels and the cancellation stay with the execution,
// Reversible arrives from the run's MarkReversible-written flag, and the
// generation is the incarnation the projection describes.
func registrationOf(scopeID string, entry Run, reversible bool, gen uint64) Registration {
	return Registration{
		ScopeID:      scopeID,
		Generation:   gen,
		Role:         entry.Role,
		Label:        entry.Label,
		Feature:      entry.Feature,
		Workdir:      entry.Workdir,
		StartedAt:    entry.StartedAt,
		Reversible:   reversible,
		Redirectable: entry.Redirectable,
	}
}

// forgetFinishedLocked drops a finished record for a scope that is
// registering again: one scope is one line in a snapshot.
func (r *Registry) forgetFinishedLocked(scopeID string) {
	for i := range r.finished {
		if r.finished[i].ScopeID != scopeID {
			continue
		}
		r.finished = append(r.finished[:i], r.finished[i+1:]...)
		return
	}
}

// clock reads the registry clock, defaulting to time.Now for a zero-value
// Registry built outside NewRegistry.
func (r *Registry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// MarkReturned records that the generation's loop has returned. The
// registration stays live — the redirect machinery (claim, consume,
// close) works exactly through the adoption window — but steering must
// stop claiming delivery: a buffered injection the returned loop will
// never drain is a false "delivered" (PR #398 review finding).
//
// The scope's in-flight table is cleared here, not at unregister: a
// returned run still reads as running through its adoption window, but
// nothing is executing anymore, and an end event lost to the loop's
// closing broker must not leave a call aging as if hung. The lock order
// is the registry's mutex, then the table's — the same order
// InFlightCalls already establishes.
func (r *Registry) MarkReturned(scopeID string, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return
	}
	run.returned = true
	run.inflight.Clear()
}

// CancelGeneration ends exactly one generation — the live redirect's
// stop. Generation-checked on purpose: a scope-keyed Cancel can land
// after the claimed run finished and its successor registered, stopping
// the replacement that already carries the guidance (PR #398 review
// finding). Stops through Run.Handoff so the runner's halt accounting
// never reads a redirect's stop as a supervisor halt; falls back to
// Cancel for registrations that predate the split.
func (r *Registry) CancelGeneration(scopeID string, gen uint64) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.gen != gen {
		return nil, ErrNoRun
	}
	stop := run.entry.Handoff
	if stop == nil {
		stop = run.entry.Cancel
	}
	if stop == nil {
		return nil, ErrNotHaltable
	}
	stop()
	if run.entry.Done == nil {
		return nil, nil
	}
	return run.entry.Done, nil
}

// Inject delivers guidance to the run's steering channel. The send
// happens under the registry lock, and Unregister must be called before
// the channel is closed, so a send can never land on a closed channel.
// Non-blocking: a full buffer means the run is not consuming guidance. A
// run whose loop has returned (MarkReturned) is reported undelivered —
// the caller's trace-side fallback is the honest outcome (PR #398 review
// finding).
func (r *Registry) Inject(scopeID, guidance string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok || run.entry.SteerCh == nil {
		return ErrNoRun
	}
	if run.returned || run.closed {
		// The loop has returned (or the sequence closed): guidance sent
		// now is acknowledged and then discarded. Report it undelivered
		// — the caller's trace-only fallback is the honest outcome
		// (PR #398 review finding).
		return ErrNoRun
	}
	select {
	case run.entry.SteerCh <- guidance:
		return nil
	default:
		return ErrSteerBusy
	}
}

// Cancel halts the run registered under the scope (adoption plan A3):
// it invokes the registered cancellation and returns the run's quiesce
// channel — closed when the loop has returned, so the caller can wait
// before destroying anything the run was using. The channel is nil
// when the run registered no quiesce signal. Idempotent per run —
// cancelling an already-cancelled run is the same call on the same
// context, a no-op by context semantics — and a scope whose run has
// finished reports ErrNoRun, never a phantom halt.
func (r *Registry) Cancel(scopeID string) (<-chan struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok {
		return nil, ErrNoRun
	}
	if run.entry.Cancel == nil {
		return nil, ErrNotHaltable
	}
	run.entry.Cancel()
	if run.entry.Done == nil {
		return nil, nil
	}
	return run.entry.Done, nil
}

// Haltable reports whether a halt would reach a run: a live registration whose
// run carries a cancellation. It is the non-mutating half of Cancel, and the
// ordering device a stop needs (yaah-vaxv): a stop's record must be appended
// BEFORE the cancel, because the cancel is what makes the halted run settle and
// a settling run drains its own scope's records — so a producer asks this
// question first, records while the answer still holds, and only then cancels.
// Recording after a successful Cancel is too late by construction, and it is the
// narrow race that loses a halt's record from its own run's envelope.
func (r *Registry) Haltable(scopeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	return ok && run.entry.Cancel != nil
}

// Has reports whether a run is registered for the scope.
func (r *Registry) Has(scopeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.runs[scopeID]
	return ok
}

// Entries returns the LIVE registrations, sorted by scope ID — the snapshot's
// metadata view, and the same ordering ScopeIDs reports. The channels stay
// behind: a Registration cannot deliver guidance or cancel anything.
func (r *Registry) Entries() []Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Registration, 0, len(r.runs))
	for id, entry := range r.runs {
		reg := registrationOf(id, entry.entry, entry.reversible, entry.gen)
		reg.StillRunning = true
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScopeID < out[j].ScopeID })
	return out
}

// LiveRegistration returns the scope's LIVE registration — the same projection
// Entries makes, for one scope — and false when no run is registered. It is the
// single-scope read a supervisor needs to re-validate a fact it took from a
// snapshot, which can be an interval old: the shipment of a redirect claim is
// the case that matters (yaah-3l34), where Redirectable and Reversible decide
// whether a claim may be made at all. The reported StillRunning is always true
// — finished runs live in the bounded tail, not here — and the channels stay
// behind, so the returned value steers and cancels nothing.
func (r *Registry) LiveRegistration(scopeID string) (Registration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok {
		return Registration{}, false
	}
	reg := registrationOf(scopeID, entry.entry, entry.reversible, entry.gen)
	reg.StillRunning = true
	return reg, true
}

// RecentlyFinished returns the finished registrations still in the bounded
// window, oldest first. The slice is a copy: a caller cannot reach back into
// the registry through it.
func (r *Registry) RecentlyFinished() []Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Registration, len(r.finished))
	copy(out, r.finished)
	return out
}

// SnapshotEntries returns live and finished registrations under ONE lock.
// Live come first (a supervisor can still steer them), sorted by scope ID;
// finished come newest first, so an agent cap that drops entries always
// drops the OLDEST finished runs before one that just ended. Entries() and
// RecentlyFinished() cannot be combined by a caller: a run that unregisters
// between two acquisitions would appear in both slices (Qodo review).
func (r *Registry) SnapshotEntries() []Registration {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := make([]Registration, 0, len(r.runs))
	for id, entry := range r.runs {
		reg := registrationOf(id, entry.entry, entry.reversible, entry.gen)
		reg.StillRunning = true
		live = append(live, reg)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ScopeID < live[j].ScopeID })
	// finished is oldest-first; prepend newest-first without mutating it.
	out := make([]Registration, 0, len(live)+len(r.finished))
	out = append(out, live...)
	for i := len(r.finished) - 1; i >= 0; i-- {
		out = append(out, r.finished[i])
	}
	return out
}

// ScopeIDs returns the registered run scope IDs, sorted — the
// supervisor tool's status bridge report.
func (r *Registry) ScopeIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.runs))
	for id := range r.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
