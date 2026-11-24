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
	"github.com/goatherdai/yaah/internal/memory"
	"github.com/goatherdai/yaah/internal/permissions"
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

	// now mints dispatch timestamps for the sub-agent trace ID. Nil means
	// time.Now; tests pin it to force two dispatches into the same clock
	// tick and prove the per-dispatch sequence keeps their scope keys
	// apart.
	now func() time.Time
}

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

		outLimit := resolveOutputLimit(params.OutputLimit, opts.subCfg, role, opts.outputLimit)

		sysPrompt := buildSubAgentSysPrompt(opts, role, profile, jsonMode, params.Workdir, subConf)

		// Record the role's directives on the sub-agent span so traces
		// show which policy statements this child received (if any).
		if ds := opts.subCfg.Roles[string(role)].Directives; len(ds) > 0 {
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				span.SetAttributes(attribute.String("subagent.directives", strings.Join(ds, " | ")))
			}
		}

		route := resolveDispatchRoute(opts, role, params, profile)
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
		capturePreRun := func(captureCtx context.Context) {
			preRunOK = false
			if subScope == nil || subScope.Sandbox() == nil {
				return
			}
			state, err := subScope.Sandbox().Capture(captureCtx)
			if err != nil {
				slog.Warn("live revert: pre-run workspace capture failed; a revert handoff will have no snapshot to restore",
					"scope", subScope.ID(), "err", err)
				return
			}
			preRunState = state
			preRunOK = true
		}

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
			capturePreRun(ctx)
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

		// Inner per-run segment: builds ONE loop on the shared bridge,
		// runs it, and performs that run's complete post-run accounting
		// gauntlet — transcript/stop/denials capture, usage and
		// confinement facts, the integrity gates, output budgeting, and
		// span attributes. The outer sequence below calls it exactly
		// once today; it is the seam yaah-8gxg.3 re-enters on the SAME
		// scope and steerCh after a live redirect.
		runOnce := func(ctx context.Context, prompt string) (string, error) {
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
				break
			}
			// Live revert (yaah-1050.1.3): roll the workspace back to the
			// PREVIOUS generation's pre-run snapshot BEFORE the successor
			// loop starts — the rollback that separates revert from plain
			// redirect (the LLM-side handoff above is identical). Then
			// re-capture, so the successor's own pre-run snapshot is the
			// rolled-back workspace. A missing substrate or snapshot
			// skips the rollback with a note: a revert must never fail
			// the run it handsoff.
			revertFailed := false
			if req.Revert {
				// Ownership of the workspace this rollback would touch
				// (review finding #5): only an isolated worktree belongs
				// to THIS dispatch. A container or shared-root sandbox is
				// shared with other writers — a wholesale Apply would roll
				// THEIR changes back too.
				sandboxOwned := subScope != nil && subScope.Sandbox() != nil && subScope.Sandbox().Capabilities().Isolated
				switch {
				case subScope == nil || subScope.Sandbox() == nil:
					slog.Warn("live revert: workspace rollback skipped — the sub-agent scope has no sandbox (isolated/container sessions have no per-sub-agent rollback substrate); the successor loop starts on the workspace as it is",
						"scope", bridgeScopeID)
				case !sandboxOwned:
					// A shared sandbox is not ours to roll back: the
					// snapshot covers the full tree and applying it would
					// remove OTHER writers' changes made after capture
					// (PR #398 review finding).
					slog.Warn("live revert: workspace rollback skipped — this dispatch shares its sandbox with the root workspace and a full-tree rollback could remove other writers' changes; the successor loop starts on the workspace as it is",
						"scope", bridgeScopeID)
				case !preRunOK:
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
			capturePreRun(ctx)
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
