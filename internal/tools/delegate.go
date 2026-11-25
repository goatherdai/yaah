package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/dispatch"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/permissions"
	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// DelegateTool spawns a sub-agent with a restricted tool set to complete a task.
//
// The tool supports an optional role (worker/reviewer/planner) that
// selects the sub-agent's tool set, iteration budget, and default
// timeout. Optional timeout_seconds and max_iterations override the
// role's defaults. When a sub-agent times out or is cancelled, the tool
// returns a structured JSON result rather than a hard error so the
// parent agent can decide whether to retry or move on.
//
// Verification scope is a role-prompt policy, not a tool-enforced rule:
// developer dispatches run targeted package checks and the parent runs the
// full gate suite on the merged result. See the "Verification scope" section
// of the developer role prompt for the current policy text.
//
// Background mode (background: true) dispatches the sub-agent in a
// goroutine and returns immediately. Results are delivered via
// BackgroundNotifier.
type DelegateTool struct {
	Runner jobs.TaskRunner

	// ResolveTimeout returns the effective default deadline for a call
	// when the model did not supply timeout_seconds. It receives the
	// parsed params so it can vary by role. May be nil (no default
	// timeout). The caller is expected to fold in role/profile/config
	// defaults; DelegateTool clamps any per-call override to the schema
	// bounds before this is consulted.
	ResolveTimeout func(params jobs.SubAgentParams) time.Duration

	// RoleNames is the active set of sub-agent role names known to the
	// registry. When non-empty the schema's role enum is built
	// dynamically so user-defined roles are visible to the model.
	// When empty the legacy static schema is used.
	RoleNames []string

	// RoleResolver, when non-nil, provides a live role-name lookup.
	// It is called at execution time to validate roles against the current
	// registry, so the delegate tool sees newly created roles immediately.
	RoleResolver func() []string

	// Delegatable, when non-nil, filters role names down to dispatch
	// targets (primary-only personas are excluded). The composition root
	// supplies subagent.DelegatableRole; nil admits every name, which is
	// the historical behavior for hand-built tools and tests.
	Delegatable func(string) bool

	// LaneForRole, when non-nil, resolves the dispatch lane for a role at
	// execution time (yaah-7yf): proposal (isolated worktree + sealed
	// delta), report (isolated worktree, plain result, auto-discarded), or
	// shared (direct on the workspace). Nil routes every dispatch to the
	// proposal lane — the historical behavior.
	LaneForRole func(role string) DelegateLane

	// RoleMutatesWorkspace, when non-nil, reports whether a role's tool set
	// can change the workspace — the same property LaneForRole routes on
	// (subagent.DispatchLaneFor). One decision needs it separately: the
	// supervised lane upgrade (supervisedLane, design §4.2) is worth its fork
	// only for a dispatch that could modify the tree, so a read-only
	// supervised dispatch keeps the shared lane and its fresh reads. Nil is
	// read as "assume it can mutate", which keeps the upgrade — the
	// conservative direction, and the pre-review behavior for hand-built
	// tools and tests.
	RoleMutatesWorkspace func(role string) bool

	// RoleDescriptions maps role name to a one-line description of what
	// the role does. Included in the delegate schema's role
	// parameter so the orchestrator can choose roles without calling
	// list_subagents. When empty no descriptions are appended.
	RoleDescriptions map[string]string

	// Tracker records file operations from sub-agent write/edit/delete
	// tools so the parent agent can detect when parallel workers touch
	// the same files. Nil means no tracking.
	Tracker *ConflictTracker

	// BackgroundJobs, when non-nil, manages asynchronous (background:
	// true) sub-agents: it owns the session-rooted context they derive
	// cancellation from (so they outlive the dispatching tool call and
	// turn), tracks them for status/cancel, attributes their usage, and
	// delivers their results as follow-ups. When nil, background mode is
	// unavailable and the tool errors if requested.
	BackgroundJobs *jobs.BackgroundJobs

	// Retained, when non-nil, routes the dispatch through the retained-run
	// primitive (Plan 7 Phase 2): the sub-agent runs in an isolated worktree and
	// its delta is sealed into a proposal the orchestrator settles with the
	// proposal tool. Nil keeps the direct-to-host lane. Background dispatches are
	// retained too — their delivered result is a proposal envelope — so there is
	// no path from this tool to the host tree while Retained is set.
	Retained *RetainedRuntime

	// Supervised, when true, runs this tool's dispatches through the
	// supervised dispatch path (design §4.2, yaah-ugtl): a dispatch that
	// lane routing would send to the SHARED lane is upgraded to the REPORT
	// lane — an isolated world, a plain result, and the world torn down
	// afterwards — so the work is revertable by construction, and the runner
	// refuses to start a dispatch whose pre-run workspace capture fails
	// instead of warning and running unrevertable. The composition root sets
	// it from supervisor config; the per-call `supervise` argument overrides
	// it for one dispatch. False (the default) changes nothing: lane routing
	// and every lane's behavior are exactly the unsupervised ones.
	Supervised bool

	// RequireReversible is the session's supervisor.require_reversible: every
	// supervised dispatch must be revertable, not merely watched. The runner
	// enforces it for the workspace it can see (it refuses a scope that owns
	// no isolated sandbox); this tool enforces the two escapes only IT can
	// see — the per-call `supervise: false` opt-out, and a NESTED dispatch,
	// whose writes land on the session tree rather than inside the parent's
	// isolated world. Off (the default) reads nothing and changes nothing.
	RequireReversible bool

	// Nested marks a delegate tool registered INSIDE a dispatch — a
	// sub-agent's own delegate tool, built by the runner — rather than the
	// session's. The distinction matters only under
	// supervisor.require_reversible: the session's tool dispatches into a
	// world the supervisor can roll back, a nested one has no proposal
	// runtime of its own and so sends its grandchild to the session tree.
	Nested bool
}

func (t *DelegateTool) Name() string { return "delegate" }

// PendingProposalScopeIDs lists the open retained proposals' scope IDs —
// the scope reaper's live-set complement: an unsettled proposal keeps its
// scope active on purpose (yaah-95ri.4).
func (t *DelegateTool) PendingProposalScopeIDs() []string {
	if t == nil || t.Retained == nil {
		return nil
	}
	return t.Retained.PendingProposalScopeIDs()
}

// LiveDispatchScopeIDs lists every scope the reaper must spare that the
// Registry cannot see: in-flight container/worktree dispatch scopes plus
// pending retained proposals (yaah-95ri.6).
func (t *DelegateTool) LiveDispatchScopeIDs() []string {
	if t == nil || t.Retained == nil {
		return nil
	}
	return t.Retained.InFlightDispatchScopeIDs()
}
func (t *DelegateTool) Description() string {
	return prompts.ToolDescription("delegate")
}

func (t *DelegateTool) Execute(ctx context.Context, args string) (string, error) {
	// Strict keys: a typo such as "backgroud", "seed_form" or "max_turn"
	// used to be ignored and silently change the dispatch (foreground
	// instead of background, fresh instead of threaded, role default
	// instead of the requested cap). Reject it with the allowed list.
	strict, err := DecodeStrictObject("delegate", args,
		"description", "prompt", "role", "timeout_seconds", "max_iterations",
		"max_turns", "json_mode", "output_limit", "background", "seed_from",
		"seed_mode", "provider", "model", "supervise")
	if err != nil {
		return "", err
	}
	strictJSON, err := json.Marshal(strict)
	if err != nil {
		return "", fmt.Errorf("delegate: invalid arguments: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(strictJSON, &raw); err != nil {
		return "", fmt.Errorf("delegate: invalid arguments: %w", err)
	}

	for _, f := range []string{"timeout_seconds", "max_iterations", "max_turns", "output_limit"} {
		if v, ok := raw[f]; ok {
			switch vv := v.(type) {
			case string:
				if n, err := coerceInt(vv); err == nil {
					raw[f] = n
				}
			case float64:
				raw[f] = int(vv)
			}
		}
	}

	fixed, _ := json.Marshal(raw)
	var params struct {
		Description    string `json:"description"`
		Prompt         string `json:"prompt"`
		Role           string `json:"role"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		MaxLoopCycles  int    `json:"max_iterations"`
		MaxTurns       int    `json:"max_turns"`
		JSONMode       bool   `json:"json_mode"`
		OutputLimit    int    `json:"output_limit"`
		Background     bool   `json:"background"`
		SeedFrom       string `json:"seed_from"`
		SeedMode       string `json:"seed_mode"`
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		// Supervise is the per-dispatch supervision override: nil follows
		// the session's posture, false opts this dispatch out, true opts it
		// in. A pointer, not a bool, because "absent" and "explicitly
		// false" are different answers (design §4.9).
		Supervise *bool `json:"supervise"`
	}
	if err := json.Unmarshal(fixed, &params); err != nil {
		return "", fmt.Errorf("delegate: invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.Description) == "" {
		return "", fmt.Errorf("delegate: description is required — a 3-5 word summary of the subtask")
	}
	if params.Prompt == "" {
		return "", fmt.Errorf("delegate: prompt is required")
	}
	if len(params.Prompt) > maxDelegatePromptBytes {
		return "", fmt.Errorf("delegate: prompt is %d bytes; the maximum is %d — split the task or summarize the context", len(params.Prompt), maxDelegatePromptBytes)
	}

	// Role is required: there is no default role. Reject empty and
	// unknown roles here so the model gets a self-correcting tool
	// result instead of spawning an unconfigured sub-agent.
	known := t.roleNames()
	if params.Role == "" {
		return "", fmt.Errorf("delegate: role is required — pick one of: %s (use list_subagents for details)", strings.Join(known, ", "))
	}
	if len(known) > 0 && !slices.Contains(known, params.Role) {
		return "", fmt.Errorf("delegate: %w — valid roles: %s", RoleNotFoundError{Role: params.Role}, strings.Join(known, ", "))
	}

	if t.Runner == nil {
		return "", fmt.Errorf("delegate: sub-agent runner not configured")
	}

	// Clamp every model-supplied numeric override to the bounds advertised
	// in the schema (delegate_schema.go) before it reaches the runner: schema
	// bounds are advisory — nothing in the transport enforces them — so a
	// runaway or injected payload can otherwise send a negative value or 10^6
	// and neutralize the deadline, iterate forever, or blank the report.
	// 0 is the "unset, the role decides" sentinel for all four params, so a
	// clamped-away value lands on 0 rather than on an invented default: only
	// values actually supplied are clamped.
	//
	// The role's min_turns / min_iterations floor is deliberately NOT applied
	// here — DelegateTool has no view of the role profile or the per-role
	// config. It is enforced downstream when the runner resolves the budget
	// (internal/agent/runner → internal/agent/budget.Resolve), where a
	// declared floor beats a per-call override and can even grow iterations to
	// protect it. That is what keeps the schema's "cannot go below the role's
	// min_turns" promise true.
	clampedTimeout := clampTimeoutSeconds(params.TimeoutSeconds)
	clampedIter := clampMaxLoopCycles(params.MaxLoopCycles)
	clampedTurns := clampMaxToolTurns(params.MaxTurns)
	clampedOutput := clampOutputLimit(params.OutputLimit)

	subParams := jobs.SubAgentParams{
		Role:           params.Role,
		TimeoutSeconds: clampedTimeout,
		MaxLoopCycles:  clampedIter,
		MaxToolTurns:   clampedTurns,
		JSONMode:       params.JSONMode,
		OutputLimit:    clampedOutput,
		SeedFrom:       params.SeedFrom,
		SeedMode:       params.SeedMode,
		Provider:       params.Provider,
		Model:          params.Model,
	}

	timeout := resolveTaskTimeout(clampedTimeout, t.ResolveTimeout, subParams)

	label := params.Role
	if params.Description != "" {
		label = label + " — " + params.Description
	}

	// Supervision posture for THIS dispatch (design §4.2, yaah-ugtl).
	// Resolved before the lane, because a supervised dispatch may upgrade
	// the lane routing chose.
	supervised, serr := t.supervisedDispatch(params.Supervise)
	if serr != nil {
		return "", serr
	}
	// supervisor.require_reversible is the operator's demand and it outranks
	// the model: neither the per-call opt-out nor a nested dispatch may spend
	// it. Both refusals fire here, before a lane is resolved or a world is
	// provisioned.
	if rerr := t.requireReversibleRefusal(params.Supervise, supervised); rerr != nil {
		return "", rerr
	}

	// Resolve the lane once: LaneForRole reads the live role registry, so a
	// second call could disagree with the first if a role is created or
	// edited mid-dispatch. Whether the role can mutate the workspace comes
	// from the same live registry, read here for the same reason.
	lane := supervisedLane(t.laneFor(params.Role), supervised, t.Retained != nil, t.roleMutates(params.Role))
	conflictLane := ConflictLaneShared
	if t.Retained != nil && lane != LaneShared {
		conflictLane = ConflictLaneRetained
	}
	subParams.Lane = conflictLane
	runCtx := WithConflictDispatch(ctx, label, conflictLane)
	// Name the same dispatch in the result envelope that the conflict evidence
	// names, so the orchestrator can tie a result to its records (yaah-was9.1).
	if cd, ok := conflictDispatchFrom(runCtx); ok {
		runCtx = jobs.WithDispatchID(runCtx, cd.ID)
	}
	// Hand the runner this dispatch's supervision answer (design §4.2):
	// jobs.SubAgentParams carries no supervision field, and the answer is a
	// caller decision — the per-call override over the session posture — not
	// a dispatch parameter. The marker always carries the RESOLVED value, in
	// both directions (review M1): a nested dispatch inherits its parent's
	// context, so a marker written only when supervised would let a parent's
	// supervision leak into a child that resolved to unsupervised, and rules
	// it never opted into would gate that child's start.
	runCtx = WithSupervisedDispatch(runCtx, supervised)

	// Background jobs cancel from the session root, not the dispatching call,
	// so ordinary context values do not flow through. Re-attach the conflict
	// dispatch (with a fresh id for the job) and the inherited permission
	// ceiling (parity W5.7) before the runner starts — a deferred dispatch must
	// not become the loophole that escapes what the parent denied.
	res, err := dispatch.Run(runCtx, dispatch.DispatchRequest{
		Description:             params.Description,
		Prompt:                  params.Prompt,
		Lane:                    lane,
		Params:                  subParams,
		Timeout:                 timeout,
		Background:              params.Background,
		Runner:                  t.Runner,
		Retained:                t.Retained,
		BackgroundJobs:          t.BackgroundJobs,
		BackgroundContextSeeder: backgroundContextSeeder(runCtx, label, conflictLane, supervised),
	})
	if err != nil {
		return "", err
	}
	return formatDispatchResult(res)
}

// classifyRunnerResult maps a runner outcome to the orchestrator-facing
// result. A non-empty result alongside an error is treated as the partial
// output the sub-agent produced before stopping.
func (t *DelegateTool) classifyRunnerResult(result string, err error, role string, timeout time.Duration, runCtx context.Context) (string, error) {
	return formatDispatchResult(dispatch.ClassifyRunnerResult(result, err, role, timeout, runCtx))
}

// formatDispatchResult renders the typed core's status into the ONE result
// envelope every lane returns (yaah-was9.1). The core owns execution and
// classification; this adapter owns only the wire shape.
//
// A completed dispatch (proposal, report, or shared) returns its envelope as a
// successful result. A dispatch that stopped early (timeout, stuck, cancelled)
// returns status=error with the partial body preserved in `result` — as a
// successful result, because the dispatch itself ran and the model decides what
// to do next. A dispatch that failed outright returns the same envelope as a Go
// error, so the error detail stays untruncated (see internal/tools/result.go).
func formatDispatchResult(res dispatch.DispatchResult) (string, error) {
	switch res.Status {
	case dispatch.DispatchCompleted, dispatch.DispatchProposal,
		dispatch.DispatchTimedOut, dispatch.DispatchStuck, dispatch.DispatchCancelled:
		if res.Status == dispatch.DispatchProposal && res.Proposal == nil {
			return "", fmt.Errorf("delegate: proposal result is missing its envelope")
		}
		return dispatch.MarshalProposalEnvelope(res.Envelope()), nil
	case dispatch.DispatchRunning:
		// The start signal is deliberately not an envelope: the completion is.
		return structuredBackgroundResult(res.JobID, res.Label), nil
	case dispatch.DispatchFailed:
		return "", errors.New(dispatch.MarshalProposalEnvelope(res.Envelope()))
	default:
		return "", fmt.Errorf("delegate: unknown dispatch status %q", res.Status)
	}
}

// maxDelegatePromptBytes bounds one dispatch prompt. The prompt is sent as a
// single user message, so an unbounded payload can defeat the context-window
// budget before the sub-agent's first turn. 64 KiB leaves room for a large
// code snippet while keeping the failure a clear tool error, not a transport
// or context overflow.
const maxDelegatePromptBytes = 64 << 10

// clampTimeoutSeconds caps the model-supplied timeout at the schema's
// 600s maximum so a runaway or injected value cannot pin a sub-agent
// open indefinitely. Any positive value below the maximum is honoured
// as-is; 0 means "use the role default" and is preserved, matching the
// schema's advertised 0–600 window.
func clampTimeoutSeconds(v int) int {
	if v > 600 {
		return 600
	}
	if v < 0 {
		return 0
	}
	return v
}

// clampMaxLoopCycles enforces the schema's 1–100 window. Values above 100
// are capped; 0 is preserved (means "use the role default").
func clampMaxLoopCycles(v int) int {
	if v > 100 {
		return 100
	}
	if v < 0 {
		return 0
	}
	return v
}

// clampMaxToolTurns enforces the schema's 1–50 window for the soft turn
// cap. Values above 50 are capped; any value below the window falls back
// to 0 ("use the role default") instead of being raised to 1, because
// max_turns=1 means "one tool turn then answer" — a meaning a negative or
// injected value must not be able to acquire. The role's own min_turns
// floor is applied later, when the runner resolves the budget.
func clampMaxToolTurns(v int) int {
	if v > 50 {
		return 50
	}
	if v < 0 {
		return 0
	}
	return v
}

// clampOutputLimit enforces the schema's 1024-byte minimum on the report
// cap. 0 and negative values mean "use the role/config default" and are
// preserved as 0; a smaller positive value is raised to the minimum so an
// injected sub-kilobyte cap cannot blank the sub-agent's report. The schema
// advertises no maximum, so none is enforced here — a generous cap costs
// nothing beyond output the run already paid for, unlike an unbounded loop.
func clampOutputLimit(v int) int {
	const minOutputLimit = 1024
	if v <= 0 {
		return 0
	}
	if v < minOutputLimit {
		return minOutputLimit
	}
	return v
}

// resolveTaskTimeout picks the effective timeout. A clamped per-call
// override wins; otherwise the role-aware resolver (if configured) is
// consulted. 0 means no timeout.
func resolveTaskTimeout(callSeconds int, resolveDefault func(jobs.SubAgentParams) time.Duration, params jobs.SubAgentParams) time.Duration {
	if callSeconds > 0 {
		return time.Duration(callSeconds) * time.Second
	}
	if resolveDefault != nil {
		return resolveDefault(params)
	}
	return 0
}

// backgroundDispatchEnvelope is the immediate result of a background dispatch.
type backgroundDispatchEnvelope struct {
	Status string `json:"status"`
	JobID  string `json:"job_id"`
	Label  string `json:"label"`
}

// structuredBackgroundResult builds a JSON payload for background sub-agent
// dispatch. The model sees this as the immediate tool result carrying a job
// id it can use with the subagent_jobs tool (status/cancel); the actual
// sub-agent output arrives later via a follow-up message.
func structuredBackgroundResult(jobID, label string) string {
	out := backgroundDispatchEnvelope{
		Status: "running",
		JobID:  jobID,
		Label:  label,
	}
	data, err := json.Marshal(out)
	if err != nil {
		return `{"status":"running"}`
	}
	return string(data)
}

func coerceInt(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// laneFor resolves the dispatch lane for a role. A nil LaneForRole keeps
// every dispatch on the proposal lane.
func (t *DelegateTool) laneFor(role string) DelegateLane {
	if t.LaneForRole == nil {
		return LaneProposal
	}
	if lane := t.LaneForRole(role); lane != "" {
		return lane
	}
	return LaneProposal
}

// roleMutates reports whether the role can change the workspace. A nil
// RoleMutatesWorkspace answers true: an unknown mutability is treated as
// mutating, so the supervised path isolates rather than trusts.
func (t *DelegateTool) roleMutates(role string) bool {
	if t.RoleMutatesWorkspace == nil {
		return true
	}
	return t.RoleMutatesWorkspace(role)
}

// supervisedDispatch resolves THIS dispatch's supervision posture (design
// §4.2, §4.9): the per-call override when supplied, the session posture
// otherwise.
//
// supervise: true asks for a supervision the session is not running, and that
// is refused rather than silently honored. A single dispatch can choose an
// isolated lane and can refuse to start without a snapshot — that is entirely
// local — but it cannot conjure the host-side tick that would actually watch
// it, so reporting success would promise supervision that does not exist. The
// design is silent on the combination; refusing it is the choice that keeps
// `supervise: true` meaning what it says.
//
// supervise: false opts this ONE dispatch out and leaves the lane routing
// untouched, so it behaves exactly as it did before this option existed.
func (t *DelegateTool) supervisedDispatch(override *bool) (bool, error) {
	if override == nil {
		return t.Supervised, nil
	}
	if !*override {
		return false, nil
	}
	if !t.Supervised {
		return false, fmt.Errorf("delegate: supervise: true needs supervision enabled for the session (an explicit supervisor.enabled: true) — one dispatch can choose an isolated, revertable lane, but it cannot start the host-side supervision that would watch it; omit supervise or set it to false to keep this dispatch unsupervised")
	}
	return true, nil
}

// requireReversibleRefusal holds the line supervisor.require_reversible draws
// against the two escapes a model can reach from inside a session, both
// refused at the dispatch boundary so no world is provisioned for them.
//
// The opt-out first: the operator turned the key on to make every dispatch in
// the session revertable, so a per-call `supervise: false` may not walk around
// it. The refusal names the key and both ways out, because the model cannot
// change config and needs the other one.
//
// Then the nested dispatch, refused for a reason the code establishes rather
// than a policy it prefers: a delegate tool registered inside a dispatch has
// no proposal runtime, so its grandchild runs on the SESSION tree — the
// parent's file tools are rooted at its isolated world, a nested tool's are
// not. Reverting the parent's scope rolls back the parent's world and never
// the session tree, so a grandchild's writes would survive the revert
// supervision promises. Until nested dispatches inherit a lane of their own
// (docs/supervisor-policy-design.md §9), refusing them is the honest answer
// under the key.
func (t *DelegateTool) requireReversibleRefusal(override *bool, supervised bool) error {
	if !t.RequireReversible {
		return nil
	}
	if t.Supervised && override != nil && !*override {
		return fmt.Errorf("delegate: %w: supervisor.require_reversible is on for this session, so no dispatch may opt out of supervision — remove \"supervise\": false (the dispatch then runs in an isolated, revertable world) or turn supervisor.require_reversible off", supervisionapi.ErrSupervisedNotReversible)
	}
	if supervised && t.Nested {
		return fmt.Errorf("delegate: %w: this delegate tool belongs to a sub-agent running inside a supervised dispatch, so its child would run on the session tree — outside the isolated world a revert of that parent rolls back; supervisor.require_reversible is on, so the nested dispatch is refused. Do this subtask in the current dispatch, or turn supervisor.require_reversible off", supervisionapi.ErrSupervisedNotReversible)
	}
	return nil
}

// supervisedLane upgrades the lane routing chose for a supervised dispatch
// (design §4.2, yaah-ugtl): under supervision a dispatch either owns an
// isolated workspace it can roll back or it never starts, so a lane that would
// run directly on the shared workspace is moved to the report lane — isolated
// world, plain result, world torn down afterwards. Only where the work runs
// changes; what comes back does not.
//
// The upgrade is for dispatches that could CHANGE the tree (roleCanMutate),
// because those are the ones a revert has anything to undo. A read-only role
// has no work to revert, and paying for it would be a regression in kind: the
// report world is forked at HEAD and torn down, so the dispatch would read the
// committed tree instead of the shared workspace's live state — exactly the
// freshness the shared lane exists to keep (docs/sub-agents.md), plus a fork
// and a teardown to buy nothing. Today's routing already sends read-only
// report roles to the shared lane and passes mutating report roles to the
// report lane, so this guard is what keeps the upgrade from firing on the one
// lane it cannot help; it still protects a lane source that disagrees with the
// role's tool set (a hand-built LaneForRole, a plugin-provided role).
//
// isolatedAvailable reports whether the session has an isolated lane at all
// (a retained runtime: worktree or per-dispatch container). Without one the
// lane stays shared and the decision moves to the runner, which refuses the
// dispatch under supervisor.require_reversible and otherwise runs it with
// Reversible false — the honest report for a host that cannot isolate.
func supervisedLane(lane DelegateLane, supervised, isolatedAvailable, roleCanMutate bool) DelegateLane {
	if supervised && isolatedAvailable && roleCanMutate && lane == LaneShared {
		return LaneReport
	}
	return lane
}

// supervisedDispatchKey marks a dispatch context as supervised (design §4.2).
// It is a context value, not a dispatch parameter, because the answer is the
// dispatching tool's decision (the per-call override over the session
// posture), and jobs.SubAgentParams would hand every caller a knob only
// supervision sets.
type supervisedDispatchKey struct{}

// WithSupervisedDispatch records ctx's supervision answer. It always writes,
// false included: the value is the RESOLVED posture of one dispatch, and a
// nested dispatch reuses its parent's context, so a set-only marker would let
// the parent's answer stand in for the child's (review M1).
func WithSupervisedDispatch(ctx context.Context, supervised bool) context.Context {
	return context.WithValue(ctx, supervisedDispatchKey{}, supervised)
}

// SupervisedDispatch reports whether ctx belongs to a supervised dispatch.
// The zero answer — an unmarked context — is "unsupervised", which is the
// behavior every dispatch had before supervision existed.
func SupervisedDispatch(ctx context.Context) bool {
	supervised, _ := ctx.Value(supervisedDispatchKey{}).(bool)
	return supervised
}

// backgroundContextSeeder returns the seeder a background job uses to
// re-attach what a job cannot inherit: it cancels from the session root, not
// from the dispatching call, so ordinary context values do not flow through.
// Re-attached are the conflict attribution (with a fresh id for the job), the
// inherited permission ceiling (parity W5.7) — a deferred dispatch must not
// become the loophole that escapes what the parent denied — and the dispatch's
// RESOLVED supervision answer, which the runner's start gate reads. Both
// directions are re-attached (review M1): a deferred supervised dispatch is
// still supervised, and a deferred opted-out one stays opted out.
func backgroundContextSeeder(runCtx context.Context, label, lane string, supervised bool) func(context.Context) context.Context {
	return func(jobCtx context.Context) context.Context {
		jobCtx = WithConflictDispatch(jobCtx, label, lane)
		jobCtx = permissions.WithInherited(jobCtx, permissions.Inherited(runCtx))
		return WithSupervisedDispatch(jobCtx, supervised)
	}
}
