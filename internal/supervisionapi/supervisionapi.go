// Package supervisionapi holds the small vocabulary the supervisor tool
// and the agent's supervision service share, without either importing the
// other. The error sentinels keep errors.Is identity across the boundary;
// Firing is the read-only summary the status view renders, Snapshot is the
// batched per-agent view the effect stream yields (yaah-1050.1.1), and the
// decision/tick types carry the supervisor policy loop's vocabulary
// (yaah-k0x1): one decider call per tick returns a list of decisions, and
// TickResult records what became of each.
package supervisionapi

import (
	"errors"
	"time"
)

var (
	// ErrNoRun means no live loop is registered for the scope.
	ErrNoRun = errors.New("no running loop registered for this scope")
	// ErrSteerBusy means the run's steering buffer is full.
	ErrSteerBusy = errors.New("the run's steering buffer is full; it is not consuming guidance")
	// ErrNotHaltable means the run registered no cancellation.
	ErrNotHaltable = errors.New("the run registered no cancellation; it is not haltable")
	// ErrAlreadyClaimed reports a Redirect whose scope and generation are
	// live but already claimed by an earlier Redirect for that same
	// generation. It is deliberately distinct from ErrNoRun: ErrNoRun
	// means the generation no longer names a live run (unregistered or
	// superseded by a successor Register), while ErrAlreadyClaimed means
	// this caller arrived second at a run that is still live — and must
	// not launch a second replacement loop for it.
	ErrAlreadyClaimed = errors.New("the run is already claimed for redirect")

	// ErrSupervisedCaptureFailed is a supervised dispatch's pre-run
	// workspace capture failing, which stops the dispatch before it starts
	// (design §4.2, yaah-ugtl): supervision promises a revert, and a
	// revert whose snapshot never landed has nothing to restore.
	//
	// It lives HERE, not in the runner that returns it, so the dispatch
	// boundary can classify it: the refusal is a deterministic
	// configuration outcome, and a retry policy must not re-provision a
	// world for it. The runner re-exports it, so errors.Is identity holds
	// on both sides.
	ErrSupervisedCaptureFailed = errors.New("supervised dispatch refused at start: the pre-run workspace capture failed")

	// ErrSupervisedNotReversible is supervisor.require_reversible refusing
	// a supervised dispatch that owns no isolated workspace (design §4.2):
	// a pure-causal fork, a container session's shared root, or a session
	// running directly on the shared workspace has nothing this dispatch
	// could roll back without touching other writers' work.
	ErrSupervisedNotReversible = errors.New("supervised dispatch refused at start: this dispatch has no isolated workspace it can roll back")
)

// RedirectHandle is a successful Redirect claim: the scope and the
// generation the claimer verified under the lock. It is inert data —
// holding it steers, cancels, and unregisters nothing — but it is the
// token the live-redirect handoff carries when it swaps loops: the
// registration still bears this generation until a successor Register
// supersedes it, so the handle records exactly which incarnation the
// claimer won.
type RedirectHandle struct {
	ScopeID string
	Gen     uint64
}

// RedirectRequest is a claimed live-redirect handoff's payload: the
// guidance the supervisor deposited for the successor loop. The claim
// itself is the registration's redirect-pending mark (set by Redirect,
// single winner via ErrAlreadyClaimed); the request is what the parent
// loop reads through ConsumeRedirect after the current run returns and
// clears when it adopts the handoff.
type RedirectRequest struct {
	// Guidance is the successor loop's first user message, deposited
	// verbatim by DepositRedirect after the claim.
	Guidance string
	// Revert marks a live revert (yaah-1050.1.3): when true, the
	// adoption loop rolls the sub-agent's workspace back to the
	// pre-run snapshot before starting the successor loop on the same
	// scope. When false the handoff is a plain redirect — the
	// workspace is left exactly as the cancelled run left it.
	Revert bool
}

// Firing is one aggregated rule firing: one per rule, scope, and kind, so
// the count is the aggregate and the map it is read from stays bounded.
type Firing struct {
	Rule  string
	Scope string
	Kind  string
	Count int

	// Command and Reason carry the MOST RECENT blocking deny's detail
	// for this rule, scope, and kind: the shell command the guard
	// refused and the reason it gave the model. Both are empty for
	// advisory firings, which steer rather than refuse, and for a deny
	// that carried no command. Both are bounded (the command is
	// model-authored text, so it is truncated before it is stored) and
	// both describe only the latest refusal — an earlier one is
	// summarised by the count, not by a line of its own.
	Command string
	Reason  string
}

// ToolCallDigest is one tool call in an agent's trailing trail, paired from
// the trace's declaration and its capture. Args and Error are model-authored
// text, so both are bounded — and rendered single-line — before they leave
// the trace reader: a snapshot is a summary, and one megabyte of shell
// command is not.
type ToolCallDigest struct {
	Name string
	Args string
	// Paired reports whether the call's capture was observed. The trace
	// middleware appends both halves only AFTER a call returns, so an
	// unpaired declaration normally means the capture append failed — not
	// that the call is still running. Success says nothing about it.
	Paired  bool
	Success bool
	Error   string
}

// MaxFeatureRunes caps a snapshot's feature description, counted in runes
// so a multi-byte summary cannot exceed the cap. The design's §4.2 carries
// the paper's "feature description (≤ 350 chars)" as a rune count: a
// character limit that a byte cap would break on the first non-ASCII
// summary.
const MaxFeatureRunes = 350

// InFlightCall is a tool call that has started and has not returned.
type InFlightCall struct {
	Name string
	// Args is the abbreviated argument text the start event carried,
	// bounded by the snapshot's existing args cap.
	Args string
	// Age is the time since the call started, at the snapshot's time.
	Age time.Duration
}

// AgentSnapshot is one agent's compact, batched state as the supervisor sees
// it: who is running, what it has done lately, and what it has touched.
//
// It is READ-ONLY by construction. The type carries no method, field, or
// channel the builder could use to reach the worker's messages, its tool
// registry, or its loop — so producing a snapshot cannot perturb the
// context it summarizes. That is the property the increment exists to
// establish, and it is why the builder takes a trace reader rather than an
// agent.
//
// Per-action TIME is not available: the kernel's read surface returns facts
// without timestamps or durations (an EffectEvent carries one, a retained
// record does not). StartedAt/FinishedAt are the run's registration clock,
// so an age is the age of the RUN, not of its last call; a per-action clock
// needs a write-path stamp and is future work.
type AgentSnapshot struct {
	ScopeID string
	// OwnerID is the trace owner the facts were read under: the scope ID
	// with its "scope:" prefix stripped, which is the sub-agent's session
	// ID (the runner records the scope under exactly that trace owner).
	OwnerID string
	Role    string
	// Label is the caller-supplied dispatch label. The dispatch runner
	// registers none today (SubAgentParams carries no label), so it is
	// empty for every dispatch; a labelled registration fills it without
	// changing this vocabulary.
	Label string
	// Feature is the snapshot's feature description: the dispatch label
	// when one was supplied, otherwise a summary of the dispatch prompt —
	// trimmed, whitespace-collapsed, and clipped to MaxFeatureRunes runes.
	// What the supervisor reads to decide whether the agent's work still
	// matches its task.
	Feature string
	Workdir string

	// StillRunning distinguishes a live registration from one that has
	// finished within the snapshot's recent-finished window. A finished
	// agent's facts stay readable — that is the point of keeping it — but
	// nothing will append to them.
	StillRunning bool
	StartedAt    time.Time
	// FinishedAt is zero while the run is live.
	FinishedAt time.Time

	// Reversible and Redirectable are fixed by the registration, not
	// derived: Reversible says a pre-run workspace capture exists, so a
	// revert can roll the scope back to it (marked after the capture
	// succeeds, never at registration); Redirectable is false for the
	// root scope, which a redirect must never target. The decider reads
	// both before choosing an action, so a revert on a non-reversible
	// scope is dropped rather than proposed.
	Reversible   bool
	Redirectable bool

	// LastAction is the kind label of the most recent tool call, and
	// LastArgs its bounded argument text. Empty when the agent has called
	// no tool yet.
	LastAction string
	LastArgs   string

	// Trail is the agent's trailing tool calls, oldest first, capped.
	Trail []ToolCallDigest
	// ModifiedFiles is the file set the agent's successful mutating calls
	// named, in first-write order, capped. It is derived from the call
	// arguments the data plane already records — it is not a diff, so a
	// file written and then deleted still appears, and a mutating tool
	// outside the reader's known set contributes nothing.
	ModifiedFiles []string

	// InFlight is the calls that have started and not returned, oldest
	// first, with their age at the snapshot's time. Fed from the host's
	// tool-start events and held in memory only — a scope that is no
	// longer running reports nil, which is what makes a call cut off by
	// cancellation disappear.
	InFlight []InFlightCall

	// Calls and Errors are totals over the facts the reader observed —
	// not just the capped trail — so an agent with 300 calls and 4
	// failures reports 300/4 while showing its last 25 calls.
	Calls  int
	Errors int
	// ConsecutiveErrors is the paired failed calls since the last paired
	// success, derived over the full fact stream — not the capped trail —
	// so a streak that started outside the trailing window still shows.
	// A paired success resets it to zero, and an unpaired call (a pending
	// call, or a capture append that failed) neither counts nor resets:
	// it was not observed to fail.
	ConsecutiveErrors int
	// Through is the highest trace ordinal observed for the owner — the
	// cursor a later incremental read resumes from — and -1 when the
	// owner path held no facts (a run that has not appended anything, or
	// a scope whose trace owner could not be read).
	Through int

	// Note carries the honest reason the data is missing or partial —
	// a trace read failure, or a truncated read. Empty when the snapshot
	// is complete.
	Note string
}

// Snapshot is the whole batch: every agent the supervisor can see right now,
// plus the count it could not fit.
type Snapshot struct {
	At      time.Time
	Agents  []AgentSnapshot
	Dropped int
}

// SnapshotSource is the supervisor tool's consumer-side view of the effect
// stream: no error return, because a snapshot is best-effort by contract —
// an unreadable trace yields an agent with a Note, never a failed poll. The
// concrete implementation lives in internal/agent/supervision; the tool does
// not import the agent family back.
type SnapshotSource interface {
	Snapshot() Snapshot
}
