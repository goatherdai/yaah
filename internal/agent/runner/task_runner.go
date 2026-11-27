package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"
	"github.com/goatherdai/goatherd-runtime/sandbox/confinement"
	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/agent/llm"
	"github.com/goatherdai/yaah/internal/agent/pipeline"
	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/dispatch"
	"github.com/goatherdai/yaah/internal/memory"
	"github.com/goatherdai/yaah/internal/permissions"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// taskRunnerOpts holds the shared state needed to build sub-agent loops.
// It is captured by every makeTaskRunner closure so nested sub-agents
// (planner -> worker) inherit the same provider, prompt base, and config.
type taskRunnerOpts struct {
	provider agent.Provider
	// providerName labels the planner's provider in sub-agent error
	// envelopes when no explicit sub-agent provider is configured.
	providerName  string
	systemPrompt  string
	modelName     string
	db            *memory.DB
	parentSession string
	subCfg        config.SubAgentConfig

	// subNetwork is the egress posture for confined dispatches (Plan 7 Phase 3),
	// lowered from agents.subagent.network with the session broker's port.
	//
	// The zero value means allow_all — the config default, and the behaviour of
	// every session before this key existed — and resolveSubAgentConfinement
	// normalizes it to an explicit allow_all rather than handing an empty mode to
	// the runtime (whose own zero policy fails closed to deny_all). A caller that
	// omits it therefore gets today's behaviour, not a dispatch that refuses
	// wherever the host cannot jail.
	subNetwork confinement.NetworkPolicy

	// subAgentProvider and subAgentModel override the planner's
	// provider/model for sub-agent loops. When nil/empty, sub-agents
	// inherit the planner's provider and model.
	subAgentProvider agent.Provider
	subAgentModel    string

	// OtelEnabled propagates OpenTelemetry tracing to sub-agent loops
	// so their tool calls appear as child spans in the trace waterfall.
	OtelEnabled bool

	// OtelVerbose propagates verbose trace recording to sub-agent loops
	// so their model content/reasoning is captured when the parent has
	// verbose tracing enabled.
	OtelVerbose bool

	// tracker records file operations from sub-agent write/edit/delete
	// tools so the parent agent can detect parallel-worker conflicts.
	tracker *tools.ConflictTracker

	// subContextWindow is the parent agent's context window halved for
	// sub-agent use, with a floor of 32000. Zero disables compaction.
	subContextWindow int

	// outputLimit caps the final synthesized sub-agent result in bytes.
	outputLimit int

	// estimateFactor is the preflight token estimate multiplier inherited
	// from the parent config.
	estimateFactor float64

	// providerMap is the full set of configured providers, needed for
	// per-role provider resolution by name. When nil/unset, per-role
	// provider overrides fall through to the global override.
	providerMap map[string]config.Provider

	// defaults carries the top-level agent defaults so sub-agent loops
	// inherit loop-detection thresholds, retry policies, compaction
	// tuning, and concurrency caps from the parent config.
	defaults config.Defaults

	// toolSpillDir is the directory where oversized sub-agent tool
	// results are spilled to disk. Injected from the composition root so
	// sub-agents inherit the parent's spill directory instead of reaching
	// into config for paths. Empty disables spilling.
	toolSpillDir string

	// pathValidator, when non-nil, confines the sub-agent's
	// file-accessing tools to the session workspace, matching the
	// parent session's --workspace policy.
	pathValidator *tools.PathValidator

	// workspace, when non-nil, is the isolated session's sandbox
	// workspace (plan §1.3, decision D2): every sub-agent inherits the
	// session's one sandbox instead of running host-side. Mutually
	// exclusive with pathValidator — the wiring installs exactly one
	// substrate per session.
	workspace tools.Workspace

	// rootScopeID is the session's goatherd root scope (adoption plan
	// D1): sub-agent scopes fork from it. Empty falls back to the legacy
	// flat per-dispatch scope.
	rootScopeID string

	// runs is the session's supervision run registry (adoption plan A2),
	// injected by the wiring alongside the root scope — session-owned,
	// per the repo's no-globals convention. Nil when tracing is off:
	// dispatches then have no injectable bridge.
	runs *supervision.Registry

	// scopeManager is the session's scope registry (plan 07, H4),
	// injected by the wiring like the run registry — sub-agent scopes
	// fork through it without reaching through package globals. Nil
	// when tracing is off: dispatches then run without scopes.
	scopeManager *goatherd.ScopeManager

	// maxDispatchScopes caps concurrently active dispatch scopes; 0 =
	// jobs.DefaultMaxDispatchScopes (yaah-95ri.4).
	maxDispatchScopes int

	// resumedScopes is the set of scope IDs startup recovery re-admitted
	// from the trace (yaah-95ri.7). Nothing runs under them in this
	// process, so the dispatch cap must not count them — see
	// jobs.DispatchScopeCount. Nil when no recovery adopted anything.
	resumedScopes map[string]bool

	// traceStore is the session's Goatherd trace store (plan 07, H4),
	// injected alongside the manager: the sub-agent loops' goatherd_trace
	// middleware writes through it. Nil when tracing is off.
	traceStore pipeline.TraceSink

	// guardCheck is the supervision service's blocking pre-dispatch
	// check (adoption plan B2), injected by the wiring: each sub-agent
	// loop's pipeline consults it before executing a tool call, and a
	// non-empty reason denies the call — sub-agent loops run at
	// ApprovalMode "allow", so this is the net their calls pass
	// through. Nil when tracing is off or the guard is disabled in
	// config; calls then dispatch unchanged.
	guardCheck func(ownerID, toolName, argsJSON string) string

	// guardDenyRec records a refusal the dispatching role's own
	// allow_commands list made, so a per-role deny is as legible on the
	// supervision status and trace surfaces as the global guard's. Nil
	// records nothing and changes no verdict.
	guardDenyRec supervision.DenyRecorder

	// resolveProviderByName resolves a configured provider by name. It
	// is injected from the cmd layer because provider construction
	// (makeProvider) is CLI wiring that does not belong here. May be nil;
	// a nil resolver means per-role provider overrides fall through to
	// the planner's provider.
	resolveProviderByName func(pmap map[string]config.Provider, name string) agent.Provider

	// resolveTaskRunner resolves a configured backend/agent name to a
	// sub-agent runner. Nil means only the in-process loop is available.
	resolveTaskRunner func(backend, agent string) tools.TaskRunner

	// now mints dispatch timestamps for the sub-agent trace ID. Nil means
	// time.Now; tests pin it to force two dispatches into the same clock
	// tick and prove the per-dispatch sequence keeps their scope keys
	// apart.
	now func() time.Time

	// requireReversible refuses a SUPERVISED dispatch whose workspace it
	// cannot roll back (supervisor.require_reversible, design §4.2): the
	// scope owns no isolated sandbox, so a revert would roll other
	// writers' work back or nothing at all. Whether a dispatch is
	// supervised is per-dispatch (tools.SupervisedDispatch on the dispatch
	// context); this flag is the session's demand, never a supervisor in
	// its own right — with supervision off nothing consults it.
	requireReversible bool

	// supervised is THIS dispatch's RESOLVED supervision posture: the
	// per-call `supervise` override over the session posture, resolved by
	// the dispatching tool and read off the dispatch context at the start
	// of the run. It is per-dispatch state rather than construction state
	// so a nested delegate tool can be handed the same answer the dispatch
	// that built it resolved (review M1) — the child of a supervised
	// dispatch is supervised, the child of an opted-out one is not.
	supervised bool

	// superviseRun is host-side supervision's start seam (yaah-0nbb, design
	// §4.8): called once per dispatch, after the bridge registered and the
	// supervised start gate passed, with the context the whole run sequence
	// shares. Its returned function is deferred, i.e. called when the dispatch
	// settles. Nil means the session runs no host-side supervision — policy
	// none and policy agent both leave it nil, agent mode driving its ticks from
	// the `supervise` tool instead.
	superviseRun SuperviseRunHook
}

// SuperviseRunHook starts host-side supervision for one dispatch and returns the
// function that stops it (yaah-0nbb). scopeID is the dispatch's supervision
// scope — the scope the bridge registered — so a caller that wants a per-scope
// bound has it without a second seam; the batch's membership stays the session's
// whole dispatched set either way.
type SuperviseRunHook func(ctx context.Context, scopeID string) (stop func())

// dispatchClock returns the clock that stamps dispatch IDs: the test
// override when set, time.Now otherwise.
func dispatchClock(opts taskRunnerOpts) time.Time {
	if opts.now != nil {
		return opts.now()
	}
	return time.Now()
}

// subAgentTraceID mints the trace/scope owner ID for one dispatch. The
// kernel registers the sub-agent scope under this exact string
// ("scope:" + owner), so it must be unique per dispatch: two parallel
// delegate calls routinely start within one clock tick, which is why a
// timestamp alone is not a key — the same reason NewEntryID abandoned
// mem-<UnixNano>. The timestamp stays for ordering and readability in
// traces; 128 bits of crypto/rand make uniqueness structural.
func subAgentTraceID(role subagent.SubAgentRole, parent string, at time.Time) string {
	return fmt.Sprintf("sub-%s-%s-%d-%s", role, parent, at.UnixNano(), dispatchSuffix())
}

// dispatchSuffix returns 128 bits of crypto/rand, hex-encoded, matching
// newTurnID and NewEntryID. If the system entropy source is unavailable
// the timestamp is the fallback, accepting the collision risk that only
// a broken entropy source creates.
func dispatchSuffix() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}

// dispatchRoute is the provider/model a dispatch resolves to. providerName
// labels the resolved provider in error envelopes and traces; it is the
// configured name of whichever provider instance won the chain.
type dispatchRoute struct {
	provider     agent.Provider
	providerName string
	model        string
	// backend is "loop" (the in-process agent loop) or "acp"; agent names
	// the external ACP agent in the registry for the latter.
	backend string
	agent   string
}

// resolveDispatchRoute applies the parity-W1 routing chain:
//
//	per-call provider/model > config role entry (agents.subagent.roles,
//	config-authoritative) > role file > global sub-agent override >
//	parent session model > config default
//
// Provider and model resolve field-wise: a source may name only one of
// them, and the next source fills the other. Every source that names a
// provider it cannot resolve warns and falls through rather than failing
// the dispatch — a file must never be able to break a run by naming a
// provider this machine does not have.
//
// A provider switch clears the inherited sub-agent model (it may not exist
// on the new provider); the parent session's model remains the last-resort
// default, because every dispatch needs some model and the chain names no
// provider-local replacement.
func resolveDispatchRoute(opts taskRunnerOpts, role subagent.SubAgentRole, params tools.SubAgentParams, profile subagent.RoleProfile) dispatchRoute {
	route := dispatchRoute{
		provider:     opts.subAgentProvider,
		providerName: opts.providerName,
		model:        opts.subAgentModel,
	}
	// Start with the provider that runs when no role override
	// resolves: the global sub-agent provider, else the planner's.
	if opts.subAgentProvider != nil && opts.subCfg.Provider != "" {
		route.providerName = opts.subCfg.Provider
	}
	resolve := func(name string) agent.Provider {
		if opts.resolveProviderByName == nil {
			return nil
		}
		return opts.resolveProviderByName(opts.providerMap, name)
	}
	// The config role entry is config-authoritative: it sits above the
	// role file, and an unresolvable entry falls through to it.
	rc, hasRC := opts.subCfg.Roles[string(role)]

	// Backend routing is config-authoritative and independent of provider
	// resolution: an ACP role hands the whole turn to an external agent.
	backend, acpAgent := "loop", ""
	if hasRC {
		if b := strings.TrimSpace(rc.Backend); b != "" {
			backend = strings.ToLower(b)
		}
		acpAgent = rc.ACPAgent
	}
	route.backend, route.agent = backend, acpAgent

	// providerDecided tracks whether an explicit source picked the
	// provider; the parent/global defaults only fill what nobody named.
	providerDecided := false

	// Per-call override first (parity W1.3): the call wins over every
	// file and config source.
	if params.Provider != "" {
		if p := resolve(params.Provider); p != nil {
			route.provider = p
			route.providerName = params.Provider
			providerDecided = true
			route.model = "" // a switched provider invalidates the inherited model
		} else {
			slog.Warn("sub-agent dispatch: per-call provider unknown; falling down the routing chain",
				"role", string(role), "provider", params.Provider)
		}
	}
	if params.Model != "" {
		route.model = params.Model
	}

	if hasRC && !providerDecided && rc.Provider != "" {
		if p := resolve(rc.Provider); p != nil {
			route.provider = p
			route.providerName = rc.Provider
			providerDecided = true
			route.model = ""
		}
	}
	if hasRC && rc.Model != "" && params.Model == "" {
		route.model = rc.Model
	}

	// Role-file routing (parity W1, yaah-21uk.15): the dispatch role's
	// own file, below the config role entry and above the global
	// sub-agent override.
	if !providerDecided && profile.Provider != "" {
		if p := resolve(profile.Provider); p != nil {
			route.provider = p
			route.providerName = profile.Provider
			providerDecided = true
			route.model = ""
		} else {
			slog.Warn("sub-agent dispatch: role file provider unknown; falling down the routing chain",
				"role", string(role), "provider", profile.Provider)
		}
	}
	if profile.Model != "" && params.Model == "" && (!hasRC || rc.Model == "") {
		route.model = profile.Model
	}
	if route.provider == nil {
		route.provider = opts.provider
		route.providerName = opts.providerName
	}
	if route.model == "" {
		route.model = opts.modelName
	}
	return route
}

// revertOwnsSandbox reports whether the dispatch's sub-agent scope owns
// an ISOLATED sandbox — a workspace a revert could reset wholesale
// because no other writer shares it. A shared-lane dispatch (a plain fork
// inherits the session's sandbox; a container session's root is every
// sub-agent's workspace) fails this: a full-tree Apply would remove OTHER
// writers' changes made after the capture (yaah-1050.1.3, PR #398 review
// finding #5).
func revertOwnsSandbox(subScope *goatherd.Scope) bool {
	return subScope != nil && subScope.Sandbox() != nil && subScope.Sandbox().Capabilities().Isolated
}

// revertCanRollBack is the one reversibility predicate (yaah-k0x1): a
// revert can restore this dispatch's workspace only when the pre-run
// capture succeeded AND the scope owns an isolated sandbox. It gates both
// MarkReversible calls and the adoption path's rollback, so the snapshot's
// Reversible flag and the revert's actual capability cannot drift apart:
// a dispatch on a shared sandbox proceeds with Reversible false (design
// §4.2), and a revert claimed against it is dropped as not reversible,
// never downgraded to a redirect (design §4.6).
func revertCanRollBack(subScope *goatherd.Scope, captured bool) bool {
	return captured && revertOwnsSandbox(subScope)
}

// Refusals a supervised dispatch can fail at start with. They are values so a
// caller can tell a supervision refusal from an ordinary dispatch failure
// (design §4.2, yaah-ugtl), and they are DEFINED in supervisionapi so the
// dispatch boundary can classify them without importing this package; these
// aliases keep errors.Is identity on the runner side, where they are raised.
var (
	// ErrSupervisedCaptureFailed: the pre-run workspace capture failed, so
	// the revert supervision promises has nothing to restore. Today's
	// unsupervised behavior downgrades this to a warning. The dispatch
	// boundary classifies it preflight_transient: a retry provisions a
	// fresh world and takes a fresh capture in it.
	ErrSupervisedCaptureFailed = supervisionapi.ErrSupervisedCaptureFailed

	// ErrSupervisedCaptureSkipped: a dispatch that owns a workspace, with the
	// runs registry that arms the capture present, never ATTEMPTED it. That
	// is a wiring invariant violation, so the boundary classifies it
	// configuration (final) — a retry would skip the capture again (yaah-ugtl
	// review M4). A dispatch with no registry never had the seam; its refusal
	// under require_reversible is ErrSupervisedNotReversible (review M2).
	ErrSupervisedCaptureSkipped = supervisionapi.ErrSupervisedCaptureSkipped

	// ErrSupervisedNotReversible: supervisor.require_reversible is on and
	// this dispatch owns no isolated workspace, so a revert has nothing it
	// could roll back without touching other writers' work.
	ErrSupervisedNotReversible = supervisionapi.ErrSupervisedNotReversible
)

// supervisedStart is one dispatch's start posture, as the gate reads it
// (design §4.2, yaah-ugtl). The struct exists because the deciding fields are
// two bools that are easy to confuse at a call site — "a capture was
// attempted" and "the capture succeeded" — and because the impossible
// combination (a success AND an error) then has one stated reading.
type supervisedStart struct {
	// supervised is whether THIS dispatch runs under supervision. False
	// switches the whole gate off before any other field is read.
	supervised bool
	// requireReversible is the session's supervisor.require_reversible:
	// refuse a dispatch that can never roll back, rather than run it with
	// Reversible false.
	requireReversible bool
	role              subagent.SubAgentRole
	subScope          *goatherd.Scope
	// runsAvailable records that the dispatch HAD the seam that arms the
	// capture: capturePreRun is called from inside the supervision bridge
	// block, which is entered only when the dispatch has both a scope and a
	// runs registry. It is the fact rule 2 reads alongside captureAttempted
	// (yaah-ugtl review M2): without the registry the capture was never
	// armed, so "not attempted" says nothing about the wiring.
	runsAvailable bool
	// captureAttempted records that the pre-run capture actually ran.
	// INVARIANT: a supervised dispatch that entered the bridge block — a
	// scope with a sandbox AND a runs registry — always attempts it,
	// because capturePreRun is called unconditionally right after the run
	// registers, before anything else can start the loop. A false here with
	// a sandbox and the registry present means an edit routed around that
	// call, and the gate fails closed rather than start a run nothing
	// snapshotted. A false with the registry ABSENT is not that: the seam
	// never existed for this session, which rule 3 handles (Reversible is
	// false with no capture, so require_reversible is what refuses it).
	captureAttempted bool
	// captured records that the capture succeeded; captureErr names why it
	// did not. captureErr WINS if both are set: capturePreRun clears
	// captured before it returns an error, so only a hand-assembled value
	// can set both, and reading the success first would pass a failed
	// capture.
	captured   bool
	captureErr error
}

// hasSandbox reports whether this dispatch's scope owns a workspace at all.
func (in supervisedStart) hasSandbox() bool {
	return in.subScope != nil && in.subScope.Sandbox() != nil
}

// supervisedStartRefusal decides whether a SUPERVISED dispatch may start
// (design §4.2, yaah-ugtl): a supervised dispatch either can roll back or it
// never starts.
//
// Three rules, in that order:
//
//  1. A failed pre-run capture is fatal WHERE THE CAPTURE DECIDES ANYTHING:
//     a scope that owns an isolated sandbox. There, the run would otherwise
//     proceed with Reversible false while the supervisor believes the revert
//     capability it observed at dispatch start still holds — exactly the
//     silent downgrade gap 7 of the design closes — and this applies with
//     require_reversible off too: the flag is about scopes that can never
//     roll back, not about snapshots that failed to land. A plain fork on
//     the session's SHARED sandbox is the other case: it was never revertable
//     with or without the snapshot (revertCanRollBack requires an isolated
//     world), so a transient snapshot failure there breaks no guarantee and
//     must not abort a dispatch deliberately kept shared (a read-only role,
//     mode: shared). Rule 3 still refuses it under require_reversible, and
//     without the flag it runs with Reversible false — capturePreRun's
//     warning on the record (review M-r5).
//
//  2. A dispatch that HAS a workspace, HAD the seam that arms the capture
//     (a runs registry — the supervision bridge), and never attempted it is
//     refused for the same reason, read from the other side: the gate cannot
//     tell a capture that was skipped from one that succeeded, so it treats
//     "never asked" as "no snapshot" rather than assuming the better case.
//     A scope with no sandbox at all (a pure-causal fork) is not this rule's
//     case — it has nothing to capture, and rule 3 decides it.
//
//     The runsAvailable conjunct is what makes the rule honest (yaah-ugtl
//     review M2). The capture is taken unconditionally ONLY inside the bridge
//     block, which the runner enters when the dispatch has a scope AND a runs
//     registry; a session with supervisor.enabled: true but no registry
//     (tracing never came up, a non-session construction) has a sandboxed
//     scope and a capture that was never ARMED. Reading that as a skipped
//     capture made every such dispatch fail as a final configuration error —
//     a wiring bug reported against the operator. With the registry absent
//     the dispatch proceeds and reads Reversible false, which is the true
//     statement: no snapshot was taken, so a revert has nothing to restore.
//     Rule 3 is then the rule that refuses it under require_reversible,
//     because revertCanRollBack is false without a capture.
//
//     This rule has its own sentinel (ErrSupervisedCaptureSkipped), and the
//     message no longer has to name which refusal fired. Given the guard
//     above, "never asked" can only mean the wiring that arms the capture did
//     not run although the registry was there — a deterministic bug, which the
//     dispatch boundary classifies `configuration` and does NOT retry. Rule
//     1's failure is the opposite: a real capture against a world a fresh
//     attempt may capture cleanly, so it is a retryable preflight transient.
//
//  3. With require_reversible on, a scope that owns no isolated sandbox (a
//     container session's root is every sub-agent's workspace, a shared root
//     is the session's, a pure-causal fork has none) is refused. Off, it
//     proceeds with Reversible false, which is the honest report and the
//     pre-yaah-ugtl behavior.
//
// An unsupervised dispatch returns nil before any rule, so the non-supervised
// lane is byte-for-byte unaffected.
func supervisedStartRefusal(in supervisedStart) error {
	if !in.supervised {
		return nil
	}
	if in.captureErr != nil && revertOwnsSandbox(in.subScope) {
		return fmt.Errorf("sub-agent %s: %w: %v — without the snapshot a revert has nothing to restore, so the dispatch stops here rather than run unrevertable", in.role, ErrSupervisedCaptureFailed, in.captureErr)
	}
	if in.hasSandbox() && in.runsAvailable && !in.captureAttempted {
		return fmt.Errorf("sub-agent %s: %w: the gate cannot prove this run is revertable, so it stops here", in.role, ErrSupervisedCaptureSkipped)
	}
	if in.requireReversible && !revertCanRollBack(in.subScope, in.captured) {
		scopeID := ""
		if in.subScope != nil {
			scopeID = in.subScope.ID()
		}
		return fmt.Errorf("sub-agent %s: %w: scope %s runs on a workspace this dispatch does not own (a pure-causal fork, a container session's shared root, or a session with no retained runtime) — supervisor.require_reversible is on, so give the dispatch an isolated lane or turn the key off", in.role, ErrSupervisedNotReversible, scopeID)
	}
	return nil
}

// makeTaskRunner creates a sub-agent runner that honours roles, timeouts,
// iteration caps, and nesting depth.
//
// remainingDepth bounds how many levels of nested task calls the
// returned runner may itself issue. When it reaches zero the task tool
// is omitted from the sub-loop's registry, so nesting is bounded
// structurally without relying on middleware alone. Depth is hardcoded to 1.
func makeTaskRunner(opts taskRunnerOpts, remainingDepth int) tools.TaskRunner {
	return func(ctx context.Context, prompt string, params tools.SubAgentParams) (string, error) {
		// A per-dispatch world overrides the session's substrate for this
		// dispatch only (yaah-ihfh.3). The shadow copy keeps the captured
		// closure state immutable, so a container dispatch cannot leak its
		// workspace into the next one. The workspace and the host path
		// validator are mutually exclusive by construction: the workspace is
		// the substrate, and a validator rooted on the host would gate paths
		// in a tree this dispatch never sees.
		opts := dispatchOpts(opts, ctx)
		role := subagent.SubAgentRole(params.Role)
		profile := subagent.RoleProfileFor(role)

		// Resolve routing and the output cap before any in-process preflight.
		// An external ACP agent does not use yaah's tool registry, permission
		// rules, or confinement, so those checks must not gate it.
		outLimit := resolveOutputLimit(params.OutputLimit, opts.subCfg, role, opts.outputLimit)
		route := resolveDispatchRoute(opts, role, params, profile)

		// An ACP-backed role hands the entire turn to the external agent.
		// The runner owns model/start/usage/transcript reporting, so none of
		// the in-process loop, scope, or settlement machinery applies here.
		if route.backend == "acp" {
			if opts.resolveTaskRunner == nil {
				return "", fmt.Errorf("sub-agent %s: backend acp is not wired", role)
			}
			runner := opts.resolveTaskRunner("acp", route.agent)
			if runner == nil {
				return "", fmt.Errorf("sub-agent %s: no ACP agent %q configured", role, route.agent)
			}
			// An external agent is not sandboxed by yaah's tool layer, and a
			// container workspace path is not host-visible to a child process.
			if opts.workspace != nil && !opts.workspace.Local() {
				return "", fmt.Errorf("sub-agent %s: backend acp needs a host-visible workspace", role)
			}
			// At minimum start it in the session workspace rather than the
			// host cwd. The agent's own OS access remains its trust boundary.
			if params.Workdir == "" {
				if opts.workspace != nil {
					params.Workdir = opts.workspace.WorkDir()
				} else if opts.pathValidator != nil {
					if wd := strings.TrimSpace(opts.pathValidator.WorkDir); wd != "" {
						params.Workdir = wd
					} else if root := strings.TrimSpace(opts.pathValidator.WorkspaceRoot); root != "" {
						params.Workdir = root
					}
				}
			}
			// Params.Lane only names the substrate: a report on a retained
			// world and a sealed proposal both arrive as "retained". The
			// concrete lane rides on the dispatch params/context, so refuse
			// only a true proposal.
			if dispatch.Lane(params.DispatchLane) == dispatch.LaneProposal || dispatch.LaneFromContext(ctx) == dispatch.LaneProposal {
				return "", fmt.Errorf("sub-agent %s: backend acp cannot run a proposal lane (no sealed worktree)", role)
			}
			params.OutputLimit = outLimit
			return runner(ctx, prompt, params)
		}

		// Is THIS dispatch supervised? (design §4.2, yaah-ugtl) The
		// dispatching tool resolves it — the per-call supervise override over
		// the session posture — and carries the answer on the dispatch
		// context, because SubAgentParams has no supervision field and a
		// dispatch parameter is not a caller decision. Read before the bridge
		// derives its own context, so the answer is the caller's.
		//
		// The RESOLVED answer also goes into opts, which is the per-dispatch
		// shadow copy (see dispatchOpts), so the nested delegate tool this
		// run registers resolves the way this dispatch did instead of falling
		// back to a zero posture (review M1).
		supervisedDispatch := tools.SupervisedDispatch(ctx)
		opts.supervised = supervisedDispatch

		// Permission rulesets (parity W5.7): the role's own rules apply
		// first, the parent's inherited ceiling appended AFTER so an
		// inherited deny wins over any role-file rule for the same tool
		// and resource — a ceiling narrows, never widens. The parent
		// carries the ceiling on the dispatch context (it recomputes the
		// filter from its live config per dispatch); a dispatch with no
		// ceiling inherits nothing, exactly the pre-W5.7 behavior.
		rolePermissions := withInheritedCeiling(profile.Permissions, permissions.Inherited(ctx))

		// Shell-command allowlist for this dispatch's blocking guard
		// (supervisor.rules.destructive_guard): config role entry > role
		// file > nil, and nil inherits the global default the guard
		// check already carries — the same precedence shape as
		// provider/model, with the global default in the config-default
		// seat. An explicit empty list stays deny-all. Resolved HERE
		// because the sub-agent loop's owner ID is its trace session,
		// not the role: the guard cannot look the role up when it
		// denies.
		guardAllow := opts.subCfg.Roles[string(role)].AllowCommands
		if guardAllow == nil {
			guardAllow = profile.AllowCommands
		}

		// A tool-less profile is valid — some roles (e.g. grump) are
		// designed to respond without calling any tools.

		subReg, subConf, rerr := buildSubAgentRegistry(opts, profile, role, remainingDepth, params.Workdir)
		if rerr != nil {
			return "", fmt.Errorf("sub-agent %s: %w", role, rerr)
		}

		b := resolveSubAgentBudget(params.MaxLoopCycles, params.MaxToolTurns, profile, opts.subCfg, role)
		maxIter, maxTurns := b.Iterations, b.Turns

		// Record the effective budget and its provenance on the
		// sub-agent span (subagent-turn-budget-floors §4.7) so a trace
		// answers "who set this budget to N?" directly.
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(
				attribute.Int("subagent.budget.iterations", b.Iterations),
				attribute.String("subagent.budget.iterations_source", b.IterationsSource.String()),
				attribute.Int("subagent.budget.turns", b.Turns),
				attribute.String("subagent.budget.turns_source", b.TurnsSource.String()),
			)
		}

		effectiveCW := opts.subContextWindow
		if rc, ok := opts.subCfg.Roles[string(role)]; ok && rc.ContextWindow > 0 {
			effectiveCW = rc.ContextWindow
		}

		jsonMode := params.JSONMode
		if !jsonMode && opts.subCfg.Roles[string(role)].JSONMode {
			jsonMode = true
		}
		if !jsonMode && profile.JSONMode {
			jsonMode = true
		}
		if !jsonMode && opts.subCfg.JSONMode {
			jsonMode = true
		}

		sysPrompt := buildSubAgentSysPrompt(opts, role, profile, jsonMode, params.Workdir, subConf)

		// Record the role's directives on the sub-agent span so traces
		// show which policy statements this child received (if any).
		if ds := opts.subCfg.Roles[string(role)].Directives; len(ds) > 0 {
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(attribute.String("subagent.directives", strings.Join(ds, " | ")))
			}
		}

		subProvider := route.provider
		subProviderName := route.providerName
		subModel := route.model

		// Announce the resolved model before the loop runs so the parent
		// emits its sub-agent start event with the role/model pairing,
		// and the model pointer carries the final value even when the
		// loop errors early.
		tools.WriteSubAgentModel(ctx, subModel)
		tools.NotifySubAgentStart(ctx, subModel)

		subTraceID := subAgentTraceID(role, opts.parentSession, dispatchClock(opts))
		traceContext := subAgentRetainedContext(ctx, opts, role, params.Workdir, subConf, subReg)
		traceCWD := subAgentTraceCWD(opts, params.Workdir)

		// Persist the trace-owner mapping so Goatherd facts recorded under
		// the synthetic sub-ID remain joinable to the parent session
		// (consolidate-persistence Phase 0 / finding F4).
		if opts.db != nil {
			if err := opts.db.RecordTraceOwner(subTraceID, opts.parentSession, string(role)); err != nil {
				slog.Debug("trace_owner: record failed", "owner", subTraceID, "err", err)
			}
		}

		// Create a scope for this sub-agent so the supervisor can
		// inject guidance or halt it during execution. The scope is a
		// fork of the session's root scope (adoption plan D1), so the
		// supervisor sees the true dispatch tree; the fork's sandbox is
		// the sub-agent's workspace: its own worktree when the dispatch
		// isolated one, otherwise the root's shared sandbox (nil for an
		// isolated session — the container is the substrate, isolation
		// plan D2).
		var subScope *goatherd.Scope
		if mgr := opts.scopeManager; mgr != nil {
			var cerr error
			subScope, cerr = createSubAgentScope(pipeline.ScopeSourceOf(mgr), opts, subTraceID, params.Workdir)
			if cerr != nil {
				// A scope-creation failure fails the dispatch (R1
				// review): the durable worktree record is part of the
				// supervision contract, and a sub-agent that runs
				// unsupervised against a worktree the trace cannot
				// find is exactly the orphan this exists to prevent.
				return "", fmt.Errorf("sub-agent scope: %w", cerr)
			}
			// The run's scope dies with the run (yaah-95ri.4): every
			// dispatch body — report or proposal, container, worktree or
			// direct — leaves its runner-side supervision scope discarded.
			// The retained proposal's own scope is a separate fork that
			// survives until settlement; this one bridges THIS body only.
			defer func() {
				if derr := mgr.Discard(subScope.ID()); derr != nil {
					slog.Debug("dispatch scope discard at run end", "scope", subScope.ID(), "err", derr)
				}
			}()
		}

		// Record the resolved jail placement on the sub-agent's trace (Plan 7
		// Phase 3): requested vs resolved, the backend, and the sole writable
		// root — so an advisory run is never read as jailed.
		recordConfinementFact(ctx, subScope, subConf)

		// Live revert (yaah-1050.1.3): the pre-run workspace snapshot of
		// the CURRENT generation — captured right after its bridge
		// registration, before its runOnce, and re-captured after every
		// adopted handoff, so it always names the state the workspace
		// held before the run that is about to start. A scope with no
		// sandbox (isolated/container session, pure-causal fork) has no
		// rollback substrate: preRunOK stays false and the adoption loop
		// surfaces a note instead of failing the run.
		var preRunState kerneltrace.WorkspaceState
		preRunOK := false
		captureAttempted := false
		var captureErr error
		// capturePreRun snapshots the workspace the current generation
		// starts from. It returns the capture error instead of only warning,
		// because a SUPERVISED dispatch treats it as fatal (design §4.2,
		// yaah-ugtl): the caller decides, so an unsupervised dispatch keeps
		// today's warn-and-continue behavior unchanged.
		//
		// A failed capture also CLEARS the snapshot it could not replace
		// (review M4): the successor path re-captures on the same scope, so
		// a stale state left behind by the prior generation is a tree some
		// later read could apply — rolling the workspace back to a state an
		// older incarnation started from. preRunOK already gates every
		// reader, and the state is zeroed anyway so the flag is not the only
		// thing standing between a stale snapshot and an Apply.
		capturePreRun := func(captureCtx context.Context) error {
			preRunOK = false
			preRunState = kerneltrace.WorkspaceState{}
			if subScope == nil || subScope.Sandbox() == nil {
				return nil
			}
			// Counted even when it fails: "attempted" is what the start gate
			// needs to tell a skipped capture from a successful one.
			captureAttempted = true
			state, err := subScope.Sandbox().Capture(captureCtx)
			if err != nil {
				slog.Warn("live revert: pre-run workspace capture failed; a revert handoff will have no snapshot to restore",
					"scope", subScope.ID(), "err", err)
				return err
			}
			preRunState = state
			preRunOK = true
			return nil
		}

		// The snapshot's feature description (yaah-k0x1): the dispatch
		// label when one was supplied, otherwise the dispatch prompt
		// trimmed, whitespace-collapsed, and clipped. Computed ONCE, before
		// the bridge, from the ORIGINAL prompt — a redirect successor's
		// first message is the supervisor's guidance, and the feature must
		// keep describing the task the scope was dispatched for — so the
		// registry never holds a full prompt. SubAgentParams carries no
		// label today; when one lands, it takes the label seat.
		feature := supervision.DeriveFeature("", prompt)

		// Supervision bridge (adoption plans A2/A3): the run registers
		// its steering channel and its cancellation under the scope ID,
		// so the supervisor tool's inject and halt reach this loop
		// instead of only recording trace facts. The loop runs under a
		// context derived from the dispatch's, so cancelling it stops
		// the run without touching the caller's context. The bridge —
		// steer channel, quiesce/Done, registration — is created ONCE
		// for the whole run sequence and torn down once at its end (the
		// explicit cut after the sequence, which closes Done before it
		// removes the registration — review finding #4). The deferred
		// teardown is panic safety only.
		var steerCh chan string
		var haltedBySupervisor atomic.Bool
		var quiesce chan struct{}
		var bridgeScopeID string
		var runGen uint64
		var registerBridge func() context.Context
		// The host-supervision hook's context: the dispatch's own, captured
		// before any generation replaces it, so a halt or a redirect handoff
		// (both of which cancel the CURRENT generation's context) never stops
		// host-side supervision for the workers still running. Captured
		// unconditionally at its declaration (PR #417 review finding 7): the
		// bridge guard below decides WHETHER supervision happens, not what its
		// context is, and the hook call site has its own bridgeLive check.
		superviseCtx := ctx
		bridgeLive := false
		if subScope != nil && opts.runs != nil {
			// The sequence's root context: every generation's cancellable
			// run context derives from THIS, captured before the first
			// one replaces it, so a redirect successor inherits a live
			// context — WithCancel, never WithoutCancel (finding #8) —
			// and still answers to halt, deadline, and interrupts.
			baseCtx := ctx
			// Quiesce is a property of the SEQUENCE (finding #2): one
			// channel for every generation, created here and closed ONCE
			// at the end of the sequence while the last registration is
			// still live. Nothing ever proceeds past it on a timeout.
			quiesce = make(chan struct{})
			bridgeScopeID = subScope.ID()
			// registerBridge opens a fresh generation on the SAME scope:
			// a fresh steering channel (finding #6 — the previous
			// generation's buffered guidance must not leak into the
			// successor), a fresh cancellable run context, and a
			// registration whose Cancel stops THIS generation. It returns
			// the run context of the generation it registered.
			registerBridge = func() context.Context {
				ch := make(chan string, supervision.SteerBuffer)
				steerCh = ch
				runCtx, cancelRun := context.WithCancel(baseCtx)
				opts.runs.Register(bridgeScopeID, supervision.Run{
					SteerCh: ch,
					// Handoff is the redirect's plain stop (finding #4):
					// CancelGeneration routes here, so a redirect handoff
					// never reads as a supervisor halt. Cancel below is
					// halt-only and deliberately marks the sequence.
					Handoff: cancelRun,
					Cancel: func() {
						haltedBySupervisor.Store(true)
						cancelRun()
					},
					// Closed once at the end of the run sequence, while
					// the registration is still live (review finding #4):
					// the supervisor's bounded quiesce wait before
					// kernel-side sandbox teardown selects on this.
					Done: quiesce,
					// Snapshot metadata (yaah-1050.1.1): who is running, in
					// which workspace, since when. Advisory only — the
					// supervisor's `snapshot` action reads it and nothing else
					// does. Workdir is the dispatch's resolved workdir: the
					// isolated lanes set it to the sub-agent's own worktree,
					// and a shared-lane dispatch leaves it empty because it
					// runs in the session workspace the supervisor already
					// knows.
					Role:      string(role),
					Workdir:   params.Workdir,
					StartedAt: time.Now(),
					// Feature and Redirectable (yaah-k0x1): the feature
					// description derived once above, and every sub-agent run
					// is redirectable (the root scope's registration sets
					// neither flag). Reversible is deliberately NOT set here:
					// the pre-run capture below runs after this registration,
					// so it lands through MarkReversible once it has actually
					// succeeded.
					Feature:      feature,
					Redirectable: true,
				})
				// Register stamps a fresh generation on this incarnation;
				// capture it now so the end-of-sequence teardown removes
				// exactly this run's registration (yaah-8gxg.1's
				// generation-swap model) and can never touch a successor's.
				runGen, _ = opts.runs.Generation(bridgeScopeID)
				return runCtx
			}
			ctx = registerBridge()
			// Snapshot the workspace this generation starts from, so a
			// revert claimed against THIS generation can restore it.
			captureErr = capturePreRun(ctx)
			// Reversibility is fixed once the capture succeeds (yaah-k0x1):
			// the registry's mark is generation-checked, so a capture that
			// lands after a successor registered cannot mark the replacement,
			// and a capture on a shared sandbox marks nothing — the same
			// predicate the adoption path's rollback gates on
			// (revertCanRollBack).
			if revertCanRollBack(subScope, preRunOK) {
				opts.runs.MarkReversible(bridgeScopeID, runGen)
			}
			bridgeLive = true
			defer func() {
				// Panic safety net only, mirroring the end-of-sequence
				// order (Done first, then the removal). The real
				// teardown is the explicit cut after the run sequence
				// below; on the happy path bridgeLive is already false
				// and this never fires.
				if bridgeLive {
					close(quiesce)
					opts.runs.Unregister(bridgeScopeID)
				}
			}()
		}

		// Supervised start gate (design §4.2, yaah-ugtl): a supervised
		// dispatch either owns a rollback substrate or it does not start. The
		// capture above is the reversible half — fatal here, because running
		// on would leave the supervisor's snapshot advertising a revert whose
		// snapshot never landed — and require_reversible turns a scope that
		// can never roll back (a container session, a shared root) into a
		// refusal instead of an unrevertable run. An unsupervised dispatch
		// returns nil here without reading either flag.
		if gaterr := supervisedStartRefusal(supervisedStart{
			supervised:        supervisedDispatch,
			requireReversible: opts.requireReversible,
			role:              role,
			subScope:          subScope,
			runsAvailable:     opts.runs != nil,
			captureAttempted:  captureAttempted,
			captured:          preRunOK,
			captureErr:        captureErr,
		}); gaterr != nil {
			if bridgeLive {
				// The end-of-sequence cut's order, so a redirect waiter that
				// observes Done can still find the run: Done closes while the
				// registration is live, then the generation-guarded removal.
				close(quiesce)
				opts.runs.UnregisterGeneration(bridgeScopeID, runGen)
				bridgeLive = false
			}
			return "", gaterr
		}

		// Host-side supervision hook (yaah-0nbb, design §4.8): headless mode's
		// start seam. It is called ONCE per dispatch — after the bridge has
		// registered (so the scope is a member of the batch) and after the
		// supervised start gate has passed (so a refused dispatch never starts a
		// loop that would watch a run which does not exist).
		//
		// The returned stop is DEFERRED rather than called at the
		// end-of-sequence cut below: the defer runs at this body's return, i.e.
		// after that teardown, so the loop's last tick sees the same membership
		// the dispatch envelope does — and a panic anywhere in the gauntlet still
		// joins the loop instead of leaking it.
		if bridgeLive && opts.superviseRun != nil {
			if stop := opts.superviseRun(superviseCtx, bridgeScopeID); stop != nil {
				defer stop()
			}
		}

		// Per-turn checkpointing is enabled per role (subagent.roles.<name>.
		// turn_checkpoints). When active, the checkpointer lives on this
		// sub-agent's own scope so turn rewinds never touch the supervised
		// tool's attempt-level checkpoints.
		var turnCk agent.TurnCheckpointer
		if opts.subCfg.Roles[string(role)].TurnCheckpoints && subScope != nil && subScope.Sandbox() != nil {
			turnCk = NewGoatherdTurnCheckpointer(pipeline.ScopeSourceOf(opts.scopeManager), subScope.ID(), subScope.Sandbox())
		}

		// The guard refusals THIS run records, reported after the loop so the
		// dispatch envelope can carry them (yaah-938). Both deny paths are
		// wrapped here, per run: the check and the recorder are created once at
		// wiring and shared with the parent loop, so wrapping them at the
		// service would attribute the parent's refusals to this child.
		denials := newDenialLog()
		guardCheck, guardDenyRec := denials.wrap(opts.guardCheck, opts.guardDenyRec)

		// captured accumulates every generation's supervision records, because
		// the envelope carries ONE account of the whole sequence and a drain is
		// destructive: a redirect successor that recorded its own (or nothing)
		// must add to its predecessor's records, never replace them
		// (PR #415 review finding 1). The single capture happens after the
		// sequence loop, below.
		var captured []tools.SubAgentIntervention

		// Inner per-run segment: builds ONE loop on the shared bridge,
		// runs it, and performs that run's complete post-run accounting
		// gauntlet — transcript/stop/denials capture, usage and
		// confinement facts, the integrity gates, output budgeting, and
		// span attributes. The outer sequence below calls it exactly
		// once today; it is the seam yaah-8gxg.3 re-enters on the SAME
		// scope and steerCh after a live redirect.
		runOnce := func(ctx context.Context, prompt string) (string, error) {
			// The feed seam (yaah-k0x1): THIS generation's in-flight table,
			// fetched after registerBridge stamped runGen. The observer
			// closure forwards the loop's tool start/end events into it; the
			// per-run closure is what supplies the scope, since events carry
			// none. A nil table — no bridge — leaves the observer nil and
			// the loop exactly as before.
			var eventObserver func(agent.Event)
			if bridgeLive {
				if tbl := opts.runs.InFlightTable(bridgeScopeID, runGen); tbl != nil {
					eventObserver = tbl.Observe
				}
			}
			subLoop := agent.NewSubAgentLoop(subProvider, subReg, subModel, sysPrompt, agent.SubAgentConfig{
				Permissions:            rolePermissions,
				ProviderName:           subProviderName,
				Temperature:            profile.Temperature,
				TopP:                   profile.TopP,
				MaxLoopCycles:          maxIter,
				MaxToolTurns:           maxTurns,
				BudgetIterationsSource: b.IterationsSource.String(),
				BudgetTurnsSource:      b.TurnsSource.String(),
				MaxRetries:             opts.defaults.MaxRetries,
				RetryBackoffSecs:       opts.defaults.RetryBackoffSecs,
				MaxToolConcurrency:     opts.defaults.MaxToolConcurrency,
				EvidenceGuardNudge:     opts.defaults.EvidenceGuardNudge,
				EvidenceGuardHalt:      opts.defaults.EvidenceGuardHalt,
				JSONMode:               jsonMode,
				SeedMessages:           params.SeedMessages,
				ToolResultMaxLines:     opts.defaults.ToolResultMaxLines,
				ToolResultMaxBytes:     opts.defaults.ToolResultMaxBytes,
				ToolSpillDir:           opts.toolSpillDir,
				PruneProtectTokens:     opts.defaults.PruneProtectTokens,
				PruneMinReclaim:        opts.defaults.PruneMinReclaim,
				PruneMinTurns:          opts.defaults.PruneMinTurns,
				ContextWindow:          effectiveCW,
				OtelEnabled:            opts.OtelEnabled,
				OtelVerbose:            opts.OtelVerbose,
				SessionID:              subTraceID,
				TraceStore:             opts.traceStore,
				TraceCWD:               traceCWD,
				RetainedContext:        traceContext,
				SteerCh:                steerCh,
				EventObserver:          eventObserver,
				DestructiveGuard:       guardCheck,
				ShellGuardAllow:        guardAllow,
				ShellGuardDenyRec:      guardDenyRec,

				TurnCheckpointer:      turnCk,
				TurnCheckpointEnabled: turnCk != nil,
				TurnCheckpointMax:     opts.defaults.TurnCheckpointMax,
				MaxTurnRestores:       opts.defaults.MaxTurnRestores,
			})

			result, runErr := subLoop.Run(ctx, prompt)
			// The loop has returned: nothing is executing inside the
			// scope's sandbox anymore, so steering must stop claiming
			// delivery — a buffered injection the returned loop will never
			// drain is a false "delivered" (PR #398 review finding 10).
			if bridgeLive {
				opts.runs.MarkReturned(bridgeScopeID, runGen)
			}
			// Capture the final conversation so a turn-exhausted dispatch can be
			// continued from its context instead of restarted cold. WithoutCancel
			// keeps the write alive when a timeout ended the run.
			tools.CaptureSubAgentTranscript(context.WithoutCancel(ctx), subLoop.State.Messages)
			var maxIterErr agent.MaxIterationsError
			iterExhausted := errors.As(runErr, &maxIterErr)
			tools.CaptureSubAgentStop(context.WithoutCancel(ctx), tools.SubAgentStop{
				// The loop sets TurnBudgetExhausted on the hard iteration stop too;
				// only the soft tool-turn cap means tools were stripped.
				TurnBudgetExhausted:      subLoop.State.TurnBudgetExhausted && !iterExhausted,
				IterationBudgetExhausted: iterExhausted,
				HaltedBySupervisor:       haltedBySupervisor.Load(),
				FinishReason:             subLoop.State.LastFinishReason,
			})
			// Hand the run's guard refusals to the dispatch lane's sink: the trace
			// already records them for an operator, but the orchestrator only sees
			// what the envelope carries (yaah-938). WithoutCancel keeps the write
			// alive when a timeout ended the run.
			tools.CaptureSubAgentDenials(context.WithoutCancel(ctx), denials.list())
			// Hand this run the supervisor's account of it: every decision and
			// rules action that named this scope, in tick order (yaah-vaxv,
			// design §4.10). The trace already holds each record as a fact
			// (supervision.ScopeRecordSink writes it where it is made); this is
			// the envelope half, the only half the orchestrator reads. Drained
			// rather than read: the run that just returned is the run those
			// records were about, and a redirect successor opens its own
			// account on the same scope. ACCUMULATED, not assigned — the
			// capture itself is once per sequence, below, because a
			// destructive drain whose result is overwritten loses those
			// records from the log and the envelope alike. WithoutCancel keeps
			// the drain alive when a timeout ended the run: a halted dispatch
			// is exactly the one whose interventions matter, and a halt's own
			// record is appended BEFORE the cancel that settled this run.
			captured = append(captured, opts.runs.DrainInterventions(bridgeScopeID)...)
			recordSubAgentUsage(context.WithoutCancel(ctx), opts.traceStore, subTraceID, opts.parentSession, string(role), subModel, params.Lane, runGen, subLoop.OwnUsage())
			// Record the jail the body actually ran under, not just the one the
			// previous fact resolved: a backend can pass its availability check and
			// still fail to establish containment at exec time.
			recordConfinementDegradedFact(context.WithoutCancel(ctx), subScope, subConf)
			// Hand that same post-run truth to the dispatch lane's sink: the
			// proposal a worktree run seals cites the enforcement the run had, and
			// the dispatch cannot see this record any other way.
			tools.CaptureSubAgentConfinement(context.WithoutCancel(ctx), subConf.report())

			// The bridge cut is deliberately NOT here: review finding #4
			// requires Done to close while the registration is still live,
			// so the teardown is the outer sequence's end-of-sequence cut
			// (close(quiesce) then UnregisterGeneration), never a
			// mid-gauntlet Unregister that left a Done waiter finding
			// ErrNoRun.

			// A supervisor halt is a deliberate stop, not a failure to
			// retry: report it as such instead of surfacing the raw
			// context-canceled noise from the cancelled run.
			if haltedBySupervisor.Load() && runErr != nil {
				runErr = fmt.Errorf("sub-agent %s halted by supervisor", role)
			}

			// Whitespace-only output is a silent failure (observed when a
			// forced final turn degenerates after tools are stripped). Turn
			// it into an explicit error so the orchestrator can retry with
			// a narrower task instead of seeing an empty result.
			if runErr == nil && strings.TrimSpace(result) == "" {
				runErr = fmt.Errorf("sub-agent %s produced no output — its final turn returned empty content (likely hit its turn cap mid-task; consider raising max_turns)", role)
			}

			// Markup-only output is a silent pseudo-success (issue #314):
			// the model emitted a tool call in a nonstandard text dialect,
			// the call never executed, and the raw markup became the report.
			// Turn it into an explicit error so the orchestrator never
			// accepts an unexecuted tool call as a completed task.
			if runErr == nil && llm.LooksLikeUnexecutedToolCalls(result) {
				runErr = fmt.Errorf("sub-agent %s returned unexecuted tool-call markup as its final report — the model emitted a tool call in a nonstandard text dialect that was never executed; treat this dispatch as failed (consider raising max_turns or reducing the role's toolset)", role)
			}

			// A zero-tool role cannot have executed anything, so a command
			// transcript with output evidence in its final report is
			// fabricated (issue #329 — the same pseudo-success class without
			// the markup). Scoped to tool-less dispatches: a role with a
			// shell quoting a transcript it really produced is legitimate.
			if runErr == nil && len(profile.Tools) == 0 && llm.LooksLikeFabricatedTranscript(result) {
				runErr = fmt.Errorf("sub-agent %s has no tools but returned a command transcript with invented output — the report is fabricated, nothing was executed; treat this dispatch as failed", role)
			}

			// Enrich errors with the sub-agent's trace so the orchestrator
			// can see exactly which tool calls led to the failure. Tracing is
			// integral, so the store config always resolves; only the profile
			// lookup decides whether there is anything to append.
			if runErr != nil {
				if profile := subagentTraceProfile(ctx, opts.defaults.TraceStoreConfig(), subTraceID); profile != "" {
					runErr = fmt.Errorf("%w\n\nSubagent trace:\n%s", runErr, profile)
				}
			}

			if outLimit > 0 && len(result) > outLimit {
				result = safeTruncateBytes(result, outLimit)
				result += "\n...[sub-agent output capped at " + formatBytes(outLimit) + "]"
			}

			// Summary budgeting: if the result exceeds 25% of the sub-agent's
			// context window, trim it to prevent sub-agent output from consuming
			// the parent's entire context headroom.
			if effectiveCW > 0 && len(result) > effectiveCW/4 {
				budget := effectiveCW / 4
				result = safeTruncateBytes(result, budget)
				result += "\n...[sub-agent output trimmed to context budget " + formatBytes(budget) + "]"
			}

			tools.AddSubAgentUsage(ctx, subLoop.State.TotalTokens)

			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(
					attribute.Int("subagent.prompt_tokens", subLoop.State.TotalTokens.PromptTokens),
					attribute.Int("subagent.completion_tokens", subLoop.State.TotalTokens.CompletionTokens),
					attribute.String("subagent.model", subModel),
				)
			}

			return result, runErr
		}

		// Outer sequence: a dispatch runs one loop per generation of its
		// bridge. Each iteration performs the inner run's complete
		// gauntlet, then checks for a claimed live redirect (yaah-8gxg.4):
		// when the supervisor claimed this generation, consume its
		// guidance, open a fresh generation on the SAME scope, and loop
		// back into runOnce. Only an unclaimed return ends the sequence,
		// and the one result the envelope carries is the FINAL run's
		// (finding #1) — a cancelled predecessor's report never leaks out.
		var result string
		var runErr error
		// A supervisor halt that lands after runOnce captured its stop
		// fact — between the capture and a redirect-adoption check —
		// would otherwise surface as the predecessor's own outcome: a
		// successful result reported as completion, or the Handoff
		// cancellation and trace dump read as an ordinary, retryable
		// failure, while the recorded stop fact still denies the halt
		// (PR #400 review finding 5). Re-capture the stop honestly and
		// report the deliberate stop; never hand back the stale result.
		lateHalt := func() {
			result = ""
			runErr = fmt.Errorf("sub-agent %s halted by supervisor", role)
			tools.CaptureSubAgentStop(context.WithoutCancel(ctx), tools.SubAgentStop{HaltedBySupervisor: true})
		}
		for {
			result, runErr = runOnce(ctx, prompt)
			if !bridgeLive {
				break
			}
			req, adopted := opts.runs.ConsumeRedirectOrClose(bridgeScopeID, runGen)
			if !adopted {
				break
			}
			// The cancelled run's return is the redirect's confirmed
			// handoff signal (finding #2): adoption happens only after
			// the current run has actually returned — never on a
			// quiesce timeout with the old Done still open.
			//
			// A supervisor halt wins over a pending redirect: the halt
			// landed while the cancelled generation was ending, so no
			// successor may start — the sequence ends and the claim is
			// dropped with it (PR #398 review finding: the flag was reset
			// below instead, erasing a real halt and running a successor
			// under it). Redirect stops no longer set the flag at all:
			// they hand off through Run.Handoff, so the flag now means
			// exactly "a supervisor halted this sequence".
			if haltedBySupervisor.Load() {
				lateHalt()
				break
			}
			// The successor gets a fresh steerCh (the old generation's
			// buffered guidance is drained away) and a fresh
			// registration, so snapshot/inject/halt reach the new loop,
			// under a fresh cancellable context derived from baseCtx.
			// The guidance becomes the successor's FIRST user message —
			// an empty deposit re-runs the original prompt rather than
			// starting a promptless loop.
			for len(steerCh) > 0 {
				<-steerCh
			}
			denials.denials = nil
			ctx = registerBridge()
			// The halt may have raced the adoption above (its cancel
			// targeted the superseded generation): re-check once the
			// fresh registration is live and end the sequence if so.
			if haltedBySupervisor.Load() {
				lateHalt()
				break
			}
			// Live revert (yaah-1050.1.3): roll the workspace back to the
			// PREVIOUS generation's pre-run snapshot BEFORE the successor
			// loop starts — the rollback that separates revert from plain
			// redirect (the LLM-side handoff above is identical). A missing
			// substrate or snapshot skips the rollback with a note: the
			// successor starts on the workspace as it is. A FAILED rollback
			// is different: it leaves the workspace possibly half-reset, so
			// the dispatch ends without a successor for explicit inspection
			// before retrying. Then re-capture, so the successor's own
			// pre-run snapshot is the rolled-back workspace.
			revertFailed := false
			if req.Revert {
				// Ownership of the workspace this rollback would touch
				// (review finding #5): only an isolated worktree belongs
				// to THIS dispatch. A container or shared-root sandbox is
				// shared with other writers — a wholesale Apply would roll
				// THEIR changes back too. The gate is revertCanRollBack,
				// the same predicate both MarkReversible sites use, so the
				// snapshot's Reversible flag and this rollback cannot drift
				// apart; the cases below only choose WHICH note to surface
				// when it does not hold.
				switch {
				case subScope == nil || subScope.Sandbox() == nil:
					slog.Warn("live revert: workspace rollback skipped — the sub-agent scope has no sandbox (isolated/container sessions have no per-sub-agent rollback substrate); the successor loop starts on the workspace as it is",
						"scope", bridgeScopeID)
				case !revertOwnsSandbox(subScope):
					// A shared sandbox is not ours to roll back: the
					// snapshot covers the full tree and applying it would
					// remove OTHER writers' changes made after capture
					// (PR #398 review finding).
					slog.Warn("live revert: workspace rollback skipped — this dispatch shares its sandbox with the root workspace and a full-tree rollback could remove other writers' changes; the successor loop starts on the workspace as it is",
						"scope", bridgeScopeID)
				case !revertCanRollBack(subScope, preRunOK): // the capture is the missing half
					slog.Warn("live revert: workspace rollback skipped — no pre-run snapshot was captured for this run; the successor loop starts on the workspace as it is",
						"scope", bridgeScopeID)
				default:
					if err := subScope.Sandbox().Apply(ctx, preRunState); err != nil {
						// A failed Apply may already have reset part of
						// the tree (the runtime resets before restore): a
						// successor must not start "as if" rolled back
						// against a partially reset workspace (review
						// finding). Surface the failure and leave the
						// workspace for explicit inspection.
						runErr = fmt.Errorf("live revert failed; the workspace may be partially reset — inspect it before retrying: %w", err)
						revertFailed = true
					}
				}
			}
			if revertFailed {
				break
			}
			// The successor generation's own pre-run snapshot. Deliberately
			// NOT fatal under supervision: the dispatch's start was already
			// gated, and a redirect/revert claimed THIS generation and
			// deposited its guidance — failing here would destroy the
			// supervisor's decision. The successor simply reads Reversible
			// false, which is the honest report; capturePreRun has already
			// warned.
			//
			// The error is discarded on purpose, and nothing is lost: the
			// closure maintains preRunOK itself (cleared on entry, set only on
			// success), so the call REPLACES the first run's outcome — a stale
			// failure cannot veto the mark below when this capture succeeded
			// (review finding L5) — and the start gate that reads captureErr
			// already ran, before the sequence. A second writer of preRunOK here
			// would be two owners for one invariant.
			_ = capturePreRun(ctx)
			// The successor's own mark (yaah-k0x1): the same predicate the
			// first run's mark used, so the flag stays honest across the
			// handoff.
			if revertCanRollBack(subScope, preRunOK) {
				opts.runs.MarkReversible(bridgeScopeID, runGen)
			}
			if req.Guidance != "" {
				prompt = req.Guidance
			}
		}

		// End-of-sequence bridge teardown, in the order review finding
		// #4 requires: Done closes while the registration is still
		// live, so a redirect waiter that observes Done can still find
		// the run — then the generation-guarded removal, so this run's
		// teardown can never remove a successor's registration.
		if bridgeLive {
			// The sequence's one capture, after every generation has drained: the
			// envelope states what the supervisor did to the dispatch as a whole,
			// in the order it happened. WithoutCancel keeps the write alive when
			// a timeout ended the run.
			tools.CaptureSubAgentInterventions(context.WithoutCancel(ctx), captured)
			close(quiesce)
			opts.runs.UnregisterGeneration(bridgeScopeID, runGen)
			bridgeLive = false
		}

		return result, runErr
	}
}

// subAgentTraceCWD mirrors buildSubAgentSysPrompt: the sandbox workdir wins
// when a workspace is installed, because the isolated lane ignores a
// model-supplied workdir and file tools resolve against the sandbox.
func subAgentTraceCWD(opts taskRunnerOpts, workdir string) string {
	if opts.workspace != nil {
		return opts.workspace.WorkDir()
	}
	return workdir
}

// subAgentRetainedContext binds a sub-agent's transcript to the dispatch's
// substrate, containment, and role tool belt. The kernel stamps it into every
// fact envelope, so authority provenance is digest-bound rather than living
// only in the mutable sessions.workspace_backend/session_prompt columns.
func subAgentRetainedContext(ctx context.Context, opts taskRunnerOpts, role subagent.SubAgentRole, workdir string, conf *subAgentConfinement, reg *tools.Registry) *kerneltrace.RetainedContext {
	substrate := "yaah.substrate.host.v1"
	containment := kerneltrace.ContainUncontained
	switch {
	case tools.DispatchWorkspace(ctx) != nil, opts.workspace != nil:
		substrate = "yaah.substrate.container.v1"
		// Match the runtime's container backend, which reports contained
		// (see sandboxfake's documented parity with containerd).
		containment = kerneltrace.ContainContained
	case workdir != "":
		substrate = "yaah.substrate.worktree.v1"
		if conf != nil && conf.Enforced {
			containment = kerneltrace.ContainContained
		}
	}
	if conf != nil && conf.Enforced && containment == kerneltrace.ContainUncontained {
		containment = kerneltrace.ContainContained
	}

	var caps []string
	if reg != nil {
		for _, name := range reg.List() {
			if name != "" {
				caps = append(caps, types.ToolSchemaRef(name))
			}
		}
	}
	sort.Strings(caps)
	return &kerneltrace.RetainedContext{
		SubstrateRef:          substrate,
		Containment:           containment,
		CapabilityWitnessRefs: caps,
		ActiveBindingRefs:     []string{"role:" + string(role)},
	}
}

// dispatchOpts returns the runtime options for one dispatch. A per-dispatch
// container world travels in the context (the retained lane owns the sandbox,
// the runner owns the registry), and it replaces the session's substrate for
// this dispatch: the workspace becomes the tools' world and the host path
// validator is dropped, because the two substrates must never race for a tool.
// Without one the captured options pass through unchanged.
func dispatchOpts(opts taskRunnerOpts, ctx context.Context) taskRunnerOpts {
	if ws := tools.DispatchWorkspace(ctx); ws != nil {
		opts.workspace = ws
		opts.pathValidator = nil
	}
	return opts
}

// withInheritedCeiling merges a role's own permission rules with the
// parent's inherited ceiling (parity W5.7): role rules first, ceiling
// appended after, so under last-match-wins the inherited deny wins on
// any overlap — a ceiling narrows a dispatch, never widens it. Either
// side may be empty; both empty returns nil.
func withInheritedCeiling(role, inherited permissions.Rules) permissions.Rules {
	if len(role) == 0 {
		return inherited
	}
	if len(inherited) == 0 {
		return role
	}
	out := make(permissions.Rules, 0, len(role)+len(inherited))
	out = append(out, role...)
	out = append(out, inherited...)
	return out
}
