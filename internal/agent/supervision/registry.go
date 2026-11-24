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

// ErrRedirectNotClaimed reports a DepositRedirect at a live,
// generation-matching registration that carries no claim: guidance must
// never ride a slot the single-winner discipline did not open, so the
// deposit is refused rather than queued against a run nobody redirected.
var ErrRedirectNotClaimed = supervisionapi.ErrRedirectNotClaimed

// steerBuffer bounds one run's undelivered guidance. Steering is
// between-turn delivery; a healthy run drains this every step.
// SteerBuffer is the per-run steering channel capacity, exported for
// the composition sites that create the channels the registry borrows.
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
}

// Registration is one run as the snapshot sees it: the scope it registered
// under, its metadata, and its lifecycle clock. It deliberately does not
// carry the run's channels — a snapshot is a read of state, never a handle
// on the execution — so it stays copyable and cannot be used to steer.
type Registration struct {
	ScopeID      string
	Role         string
	Label        string
	Workdir      string
	StartedAt    time.Time
	FinishedAt   time.Time
	StillRunning bool
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
	// gen is this incarnation's generation: a registry-wide monotonic
	// number stamped by Register. Because a later Register for the same
	// scope replaces the whole run value with a higher generation,
	// UnregisterGeneration and Redirect can compare-and-swap against a
	// specific loop incarnation rather than the bare scope name — the
	// generation-swap model that keeps an old loop's teardown or claim
	// from touching a successor.
	gen uint64
	// redirectPending records a successful Redirect claim for this
	// generation. It lives and dies with the registration, so exactly
	// one claimer wins per incarnation: the next Redirect at the same
	// generation reports ErrAlreadyClaimed.
	redirectPending bool
	// pendingRedirect carries the claimed handoff's guidance until the
	// parent loop consumes it (nil when unclaimed, or once
	// ConsumeRedirect cleared it after adoption). It lives and dies
	// with the registration like redirectPending; the mark survives the
	// consume, so a second claimer at this generation still sees
	// ErrAlreadyClaimed.
	pendingRedirect *RedirectRequest
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
	r.runs[scopeID] = &run{entry: entry, gen: r.nextGen}
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
	reg := registrationOf(scopeID, entry.entry)
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
func (r *Registry) Redirect(scopeID string, fromGen uint64) (RedirectHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok || entry.gen != fromGen {
		return RedirectHandle{}, ErrNoRun
	}
	if entry.redirectPending {
		return RedirectHandle{}, ErrAlreadyClaimed
	}
	entry.redirectPending = true
	// Open the pending slot alongside the claim: an empty request the
	// claimer fills with DepositRedirect before cancelling the run, and
	// the parent loop reads through ConsumeRedirect when the run returns.
	entry.pendingRedirect = &RedirectRequest{}
	return RedirectHandle{ScopeID: scopeID, Gen: fromGen}, nil
}

// DepositRedirect attaches the handoff payload (guidance plus the
// live-revert mark) to an already-claimed redirect as a
// compare-and-swap on the generation: the scope must still be
// registered at fromGen and the claim must still be pending. ErrNoRun
// reports an unregistered scope or a generation mismatch — the claim
// died with its registration (the run finished or a successor Register
// superseded it), so the payload has nowhere to land. ErrRedirectNotClaimed
// reports a live generation nobody claimed. The deposit is a separate
// call from the claim so the claimer can carry its own payload type
// without widening Redirect's yaah-8gxg.1 signature; the tool always
// claims first, then deposits, then cancels the current run.
func (r *Registry) DepositRedirect(scopeID string, fromGen uint64, req RedirectRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok || entry.gen != fromGen {
		return ErrNoRun
	}
	if !entry.redirectPending {
		return ErrRedirectNotClaimed
	}
	if entry.pendingRedirect == nil {
		entry.pendingRedirect = &RedirectRequest{}
	}
	entry.pendingRedirect.Guidance = req.Guidance
	entry.pendingRedirect.Revert = req.Revert
	return nil
}

// ConsumeRedirect returns the pending redirect claimed for the scope's
// generation and CLEARS the slot — the adopt half of the handoff. The
// parent loop calls it after a run returns; a true result means it must
// start the successor loop on this registration. False reports no
// pending claim at that generation (none made, already consumed, or the
// registration moved on). The redirect-pending mark survives the
// consume, so a second claimer at the same generation still reports
// ErrAlreadyClaimed until a successor Register supersedes it — one
// handoff per generation even across the adoption window.
func (r *Registry) ConsumeRedirect(scopeID string, gen uint64) (RedirectRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.runs[scopeID]
	if !ok || entry.gen != gen || entry.pendingRedirect == nil {
		return RedirectRequest{}, false
	}
	req := *entry.pendingRedirect
	entry.pendingRedirect = nil
	return req, true
}

// registrationOf projects a run's bridge surface down to its snapshot
// metadata: the channels and the cancellation stay with the execution.
func registrationOf(scopeID string, entry Run) Registration {
	return Registration{
		ScopeID:   scopeID,
		Role:      entry.Role,
		Label:     entry.Label,
		Workdir:   entry.Workdir,
		StartedAt: entry.StartedAt,
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

// Inject delivers guidance to the run's steering channel. The send
// happens under the registry lock, and Unregister must be called before
// the channel is closed, so a send can never land on a closed channel.
// Non-blocking: a full buffer means the run is not consuming guidance.
func (r *Registry) Inject(scopeID, guidance string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[scopeID]
	if !ok {
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
		reg := registrationOf(id, entry.entry)
		reg.StillRunning = true
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScopeID < out[j].ScopeID })
	return out
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
		reg := registrationOf(id, entry.entry)
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
