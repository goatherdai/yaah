# Supervisor policy design — yaah-1050.1.4

Status: **revised design** — direction decided 2026-11-24, implementation not
started. Supersedes the dedicated-supervisor-loop design of aeee7543.
Amended 2026-11-24: revert by construction and in-flight calls are folded
in; intent in the trace and a restrict action are recorded as follow-ups.

Reference: Shepherd (arXiv:2605.10913v3, §5.1 + Appendix E) and its released
loop, `shepherd-agents/shepherd-experiments`,
`exp/live-intervention/src/experiment_live_intervention/core/meta_agent_llm.py`
with `exp/live-intervention/prompts/meta_prompt_v1.txt`.

Companion reading: `docs/cooperbench-parity-audit.md` (gap #4),
`docs/supervision-parity.md`, `docs/live-redirect-redesign.md` (its
"current state" section predates the generation CAS and the redirect/revert
tools).

## 1. Actors and target

| Actor | Is an LLM | Shepherd's name | yaah's home |
|---|---|---|---|
| Human operator | no | operator | writes config and specs, reviews reports and diffs; never inside the tick loop |
| Dispatching agent | yes | meta-agent | the root agent, or any sub-agent that dispatches (a nested `delegate` is registered while depth remains) |
| Workers | yes | agents | sub-agents |
| Host | no | "orchestrator" (Appendix E) | task runner, registry, supervision service |

"Orchestrator" in the paper means the host, not an LLM. This doc avoids the
word and says *dispatching agent* and *host*. Humans never issue tool calls;
every tool call in this design is issued by a model.

**Target.** The agent that dispatched a set of workers can supervise them the
way Shepherd's meta-agent does: one snapshot per tick, a list of per-agent
decisions, three interventions — and today's unsupervised behavior is one
config key away.

Decisions:

- **D1 — no second LLM loop.** The dispatching agent's own loop is the
  meta-agent loop. The host owns the tick.
- **D2 — one step function, three callers** (§4.1): the `supervise` tool, a
  headless measurement loop, and later a workflow node.
- **D3 — headless is for measurement.** It is a stateless one-shot call with
  the paper's locked prompt, not a conversation.
- **D4 — a per-agent decision list**, as in the paper and the released code.
  There is no global cap of one intervention per tick.
- **D5 — default is today's behavior.** `policy: none` changes nothing.
- **D6 — `halt` is not a policy action** (owner decision, 2026-11-24). The
  rules engine's stop and `halt_root` are untouched.
- **D7 — single-agent redirect only.** Cross-agent scope-handoff is a
  deterministic workflow in the reference code, not an LLM action (§2).
- **D8 — revert by construction, under a flag.** A supervised dispatch is
  upgraded to an isolated lane and fails if its pre-run capture fails, so a
  supervised scope either can roll back or never starts. Enforced by
  `require_reversible`, which the measurement sets.
- **D9 — in-flight calls are visible.** Intent in the trace and a restrict
  action are follow-ups (owner decision, 2026-11-24).

## 2. The protocol being reproduced

| Element | Paper and released loop | This design |
|---|---|---|
| Tick | every 5 s; skipped when no agent is active | `supervisor.interval`, default 5s |
| Observation | one combined snapshot for all agents per tick | `Batch.Next` returns one `Snapshot` per tick |
| Decision | one LLM call per tick; a JSON list of per-agent decisions; reason ≤ 20 words; guidance ≤ 60 words; unlisted agents are `none`; 60 s call timeout; parse failure means no decisions | `Decider` returns `[]Decision` with the same bounds and failure rule |
| Throttle | cooldown of "a tick or two" stated in the prompt; `ticks_since_last_action` kept by the loop | `cooldown_ticks` (default 1), enforced by the arbiter |
| Mask and cap | `available_actions`, `max_interventions` | `supervisor.actions`, `supervisor.max_interventions` |
| Inject (code: `steer`) | one new user message; session untouched | `Registry.Inject` |
| Handoff (code: `redirect`) | abort the session, restart the same agent, files kept | `Registry.Redirect` |
| Discard (code: `revert`) | restart plus rollback to the pre-run snapshot | `Registry.Redirect` with revert set |
| Record | every decision is appended to `interventions`, executed or not | `InterventionRecord` with an outcome |
| Trigger | v1: every tick with an active agent; later versions: condition-based | `trigger: always` or `on-change` |

The paper's schema says inject/handoff/discard. The released code and yaah say
steer/redirect/revert; yaah keeps its names.

Paper reference points for yaah-1050.1.5:

- 479 pairs over 25 repos. Coop baseline 28.8%, solo ceiling 57.2%. A Sonnet
  4.6 meta-agent reached 45.3%, an Opus 4.7 meta-agent 54.7%. Workers were
  Haiku 4.5.
- Supervisor wall-clock overhead per pair: +1.4 min (Sonnet), +4.3 min
  (Opus).
- Strategy coverage is the share of pairs in which the meta-agent used a
  strategy at least once. It is per model and does not sum to 100%: steer
  26.4 / 39.2, redirect 18.7 / 31.5, revert 7.9 / 4.6 (Sonnet / Opus).
- One model call per tick across all agents. Neither the paper nor the
  released code has an "at most one intervention per tick" rule, and the
  paper publishes no interventions-per-run figure.

## 3. Current state

Verified against the tree at aeee7543. Line numbers drift; the symbols do
not.

| Capability | State | Where |
|---|---|---|
| Snapshot | caps: 50 agents, 25-call trail, 12 files, 160 B of args. Has `Calls`/`Errors` totals, `ModifiedFiles`, `LastAction`, a `Through` cursor. `Label` is empty for every dispatch. No per-action time. Every read is a full prefix probe. | `internal/agent/supervision/snapshot.go`, `internal/supervisionapi` |
| In-flight calls | not visible: declaration and capture are appended only after the call returns | `supervisionapi`, `ToolCallDigest` |
| steer | `Registry.Inject` → `SteerCh` → a new `[STEER]` user message at the next step boundary, then the compaction drain hook | `internal/agent/pipeline/steer.go` |
| redirect, revert | `Registry.Redirect` is a per-generation CAS claim. The runner adopts it through `ConsumeRedirectOrClose` after the cancelled run returns. A supervisor stop voids a pending claim. The root scope has no adoption loop. | `internal/agent/supervision/registry.go`, `internal/agent/runner/task_runner.go` |
| Revert preconditions | rollback happens only when the scope has an isolated sandbox and a pre-run snapshot; otherwise it is skipped with a warning and the successor starts on the workspace as it is | `task_runner.go` |
| Guidance | redirect and revert require non-empty guidance | `internal/tools/supervisor.go` |
| Rules engine | acts per effect event: injects guidance, can stop. On by default with tracing, even when the stanza is absent. Drops every intervention type except inject and stop. | `internal/agent/supervision/service.go`, `internal/config/supervisor.go` |
| Supervisor tool | seven free-text verbs (list scopes, inject, stop, redirect, revert, status, snapshot), registered on the root session's registry only. A 6000-byte cap bounds the tool's rendered string, not the `Snapshotter`. | `internal/tools/supervisor.go`, `cmd/yaah/wiring_stages.go` |
| Envelope | carries the final run's result, guard denials and `SubAgentStop`. No record of interventions. | `internal/jobs/context.go`, `task_runner.go` |
| Policy | no `policy` key, no decision type, no ticker | — |

Gaps this task closes:

1. **The dispatching agent cannot act as meta-agent.** A blocking `delegate`
   suspends its loop for the whole run, and the supervisor tools are only
   callable between its turns.
2. **No tick, no budgeted snapshot rendering, no decision type.**
3. **The snapshot lacks the worker's assignment, loop-state age, and
   whether revert will actually roll back.**
4. **Interventions are invisible to the dispatching agent.**
5. **Rules and decisions have no shared arbiter**, so they can steer the same
   worker twice in one window.
6. **No prompt asset.** The paper's v1 prompt is the parity baseline.
7. **Revert availability is not enforced.** A shared-lane dispatch or a
   failed pre-run capture degrades to a warning, and revert later silently
   becomes a restart.

## 4. Design

### 4.1 One step function, three callers

```go
// internal/agent/supervision
type Batch struct { /* scopes, tick state, records */ }

func (b *Batch) Next(ctx context.Context, decisions []supervisionapi.Decision) (supervisionapi.TickResult, error)
```

One `Next` call is one tick: validate and apply `decisions` against the
snapshot it was handed, wait for the next tick with zero model turns, ingest,
and return `TickResult{Snapshot, Applied, Results, Done}`.

Callers:

1. **`supervise` tool** — the dispatching agent's loop (§4.7). The default
   supervised mode.
2. **Headless loop** — a `Decider` one-shot call per tick (§4.8). For
   measurement and workflow nodes.
3. **Workflow `fanout` node** — phase 2 (§4.11).

### 4.2 Snapshot

Field-for-field with Appendix E, per agent:

| Appendix E | Source | Status |
|---|---|---|
| feature description (≤ 350 chars) | the dispatch label, or a summary of the dispatch prompt | new |
| cumulative event count | `Calls` | exists |
| consecutive-error count | derived from `Trail` statuses | new, derived |
| modified-file set | `ModifiedFiles` | exists |
| last action, and how many ticks ago | `LastAction` plus `Batch` state | age is new |
| trailing 25 tool calls (about 150 B each) | `Trail`, 160 B of args | exists |
| in-flight calls | the host's tool-start events, held in memory by the `Batch` | new |
| — | `Reversible`, `Redirectable` per scope | new |

Age is ticks since this batch last acted on the scope, kept by the `Batch`,
exactly as the released loop keeps `ticks_since_last_action`. No kernel
timestamp is needed for parity. A per-effect timestamp, which would tell
"working" from "wedged", remains the declared future work in
`internal/supervisionapi` and is not a prerequisite here.

In-flight calls are ephemeral. The host already emits a tool-start event
before execution, with the tool name and arguments. The `Batch` keeps a
per-scope table of started calls that have not returned, with an age, and
clears each on return. Nothing is persisted: a host restart loses nothing
that still exists.

`Reversible` is fixed when the dispatch starts, and under supervision it is
true by construction:

- A dispatch that lane routing would send to the shared lane is upgraded to
  the report lane (isolated worktree, plain result, auto-discarded).
- A failed pre-run capture fails the dispatch at start, instead of the
  warning it logs today.
- When the host has no isolated lane for the scope (a container session,
  §9), the dispatch fails at start under `require_reversible: true` and
  otherwise proceeds with `Reversible: false`.

`Redirectable` is false for the root scope.

### 4.3 Renderer and prompt

- `internal/supervision/render.go` turns a `Snapshot` into text under a
  token budget (default 3K, configurable). The tool and the headless decider
  use the same renderer. Scope labels are short and stable (`w1`, `w2`, …) so
  decisions can name them.
- `internal/prompts/supervision/meta_prompt_v1.md` holds the paper's prompt
  verbatim, with scope ids substituted. Headless mode uses it as the system
  prompt.
- Agent mode does not replace the agent's system prompt. The `supervise`
  tool's embedded doc carries the same heuristics in condensed form: steer is
  cheap, redirect and revert are expensive, what "stuck" means, when revert is
  justified, and the cooldown.

### 4.4 Trigger

- `always`: every tick that has a live agent. The Appendix E protocol.
- `on-change`: only when a scope advanced its `Through` cursor, errored, or
  touched a file another scope in the batch has modified. The cursor is used
  as a change signal; the read stays a prefix probe.
- `never`: no tick. This is `policy: none`. Rules are unaffected.

### 4.5 Decider and decision schema

```go
// internal/supervisionapi
type Action string // none, steer, redirect, revert

type Decision struct {
    Scope    string // scope label exactly as the snapshot renders it
    Action   Action
    Reason   string // at most 20 words
    Guidance string // at most 60 words; required for steer, redirect, revert
}

type Decider interface {
    Decide(ctx context.Context, snap Snapshot) ([]Decision, error)
}
```

- A list, with at most one decision per scope per tick. Unlisted scopes are
  `none`.
- Any error, timeout (60 s), empty response or schema violation means no
  decisions that tick. No retry.
- **Agent mode** has no `Decider` object. The dispatching agent's next model
  call produces a `supervise(decisions=[…])` call, and the tool hands the
  list to `Next`.
- **Headless mode** (`cmd/yaah`) makes one JSON-mode completion with
  `meta_prompt_v1` and `policy_model`. Its parse-fail-closed shape follows
  `parseGateVerdict` in `internal/workflow/exec/gate.go`.

### 4.6 Apply: one arbiter for rules and decisions

Per scope, precedence is stop > revert > redirect > steer.

1. **Target validation.** The scope must be in the batch's last snapshot and
   still live. The root scope is never a target. The action must be in
   `supervisor.actions`.
2. **Cooldown.** A scope acted on within `cooldown_ticks` ignores a new
   steer, redirect or revert.
3. **Preconditions.** Guidance is non-empty. Revert requires `Reversible`,
   which is fixed at dispatch start (§4.2) so the decider sees it before
   choosing. A revert on a non-reversible scope is dropped with outcome
   `not_reversible`; it is never downgraded to a redirect, because that
   would keep the damage the decider asked to undo.
4. **Cap.** `max_interventions` per batch; 0 means unlimited.
5. **Claim.** `Inject`, or `Redirect` (revert sets the revert flag). The
   runner's existing adoption loop does the rest, and an existing stop still
   wins over a pending claim.
6. **Outcome.** `applied`, `claimed`, or `dropped` with a reason. The
   `Batch` records every decision with its outcome and resets the scope's age
   on `applied` or `claimed`.

The rules engine keeps what it fires, but routes through the same arbiter:
its steers respect the cooldown, and its stops void pending decisions.

### 4.7 The `supervise` tool (agent mode)

```
supervise(
  decisions?: [ {scope, action, reason, guidance} ],  # for the snapshot last returned
  wait?: "tick" | "settled"                           # default "tick"
) → { done, snapshot, applied: [ {scope, action, outcome} ], results? }
```

- On first call it binds to the live descendants of the calling scope. One
  batch per calling loop; a second caller is refused.
- `wait: "tick"` applies the decisions, waits for the next tick, and returns
  the snapshot. `wait: "settled"` applies them, blocks until every scope in
  the batch has finished, and returns the final results with the
  intervention records — the payload `delegate` returns today.
- Dispatch pattern: `delegate(background: true)` for each worker, then
  `supervise` until `done`.
- The tool call is the tick boundary, so the agent's ordinary turn cycle is
  the meta-agent loop. One dispatching-agent model call per tick is the
  price. `trigger: on-change` and `wait: "settled"` are the levers for
  interactive sessions.
- The existing `supervisor` tool keeps its direct verbs for one-off use, and
  they route through the arbiter.

### 4.8 Headless mode

`policy: headless` starts a host loop per supervised dispatch: tick, snapshot,
`Decider.Decide`, `Next(decisions)`, until the batch settles. It holds no
conversation and has no tools. It is the meta-agent as the paper measured it:
a locked prompt under 3K tokens. `policy_model` is required, and a missing
model is a config-validation error at load time. This is the mode for
yaah-1050.1.5.

### 4.9 Config

```yaml
supervisor:
  policy: none              # none (default), agent, headless
  interval: 5s
  trigger: always           # always, on-change
  actions: [steer, redirect, revert]
  cooldown_ticks: 1
  max_interventions: 0      # 0 means unlimited
  prompt: v1
  policy_model: ""          # required when policy is headless
  require_reversible: false # fail a supervised dispatch that cannot roll back
```

- `none` is the default and is today's behavior: no tool registered, no
  batch, rules untouched.
- `require_reversible` is off for interactive use and on for measurement
  (§7).
- The rule keys are unchanged. `policy` does not inherit the rules'
  "absent means on" convention: absent means `none`.
- `delegate` gains one optional override, `supervise: true` or `false`, for a
  single dispatch.

### 4.10 Record

```go
// internal/supervisionapi
type InterventionRecord struct {
    At, Tick, Scope, Source, Action, Reason, Guidance, Outcome, Generation
}
```

`Source` is `policy`, `rules` or `root`. Records are appended to the batch
and to the trace. On settle, the dispatch envelope carries the records for
that scope next to `SubAgentStop`, by the route the guard denials already
take. The dispatching agent then knows what happened to a run before it
settles that run's proposal.

### 4.11 Workflow DSL (phase 2, seam only)

A `fanout` node gains `supervise: { mode: headless, agent }`. Headless runs
`Batch.Next` inside the engine with the one-shot decider, the way a `gate`
node's `judge` calls a model. Agent mode parks the run each tick and resumes
it with decisions through `workflow_run`. Nothing in §4.1–4.10 depends on
this.

### 4.12 Authority

The policy has intervention-only authority: inject, redirect and revert on
the non-root scopes of its own batch. No shell, no tools, no settlement. In
headless mode the decider can call nothing. In agent mode the tool call
inherits the dispatching agent's authority and nothing more.

### 4.13 What this does and does not give

Operational parity with the released loop: same tick, same snapshot fields,
same decision list, same three actions and their file semantics, same
cooldown and caps, same prompt.

Residuals:

- **A call that is still running** is now in the snapshot (§4.2). Recording
  it in the trace before it returns is a follow-up, and so is letting the
  policy tighten the guard at a tick. The blocking shell guard stays the
  pre-execution net.
- **Revert everywhere** holds by construction under `require_reversible`
  (§4.2). Two cases still differ from Shepherd's overlay scopes: a container
  session gives a sub-agent no scope of its own to roll back, and the git
  worktree backend's restore does not undo changes to ignored files.
- **The workers.** The paper ran opencode sessions on Haiku 4.5. This is
  parity of mechanism, not of score; yaah-1050.1.5 measures.
- **The coop baseline's worker-to-worker relay.**

## 5. Testing / verification

- Snapshot: field-for-field against Appendix E; a budget test on the rendered
  text with 1, 2 and 10 agents.
- Decider: scripted lists; empty, malformed, unknown scope, unknown action and
  missing guidance each yield no decisions or a recorded drop.
- Arbiter: the precedence table; cooldown; mask; cap; revert on a
  non-reversible scope is dropped and never downgraded; a rules steer inside
  the cooldown suppresses a policy steer; a rules stop voids a pending claim.
- Batch: exactly one tick per `Next`; a slow decider never applies a stale
  decision; `done` only after every scope has finished.
- In-flight calls: a started call that has not returned appears with its age
  and disappears on return.
- Revert by construction: with supervision on, a shared-lane dispatch is
  upgraded; a failed pre-run capture fails the dispatch; under
  `require_reversible` a scope with no isolated lane is refused.
- Tool: not registered under `policy: none`; binds to descendants only; the
  root scope is never in the batch; a second caller is refused.
- Envelope: records reach the dispatch result in tick order.
- Non-perturbation: a supervised run with no decisions leaves the worker's
  context and workspace byte-identical to an unsupervised run.
- Default posture: an unconfigured session registers no `supervise` and
  starts no batch.

## 6. Sequencing

1. Types (`Decision`, `TickResult`, `InterventionRecord`, `Decider`) and
   tests — `internal/supervisionapi`.
2. Snapshot additions: dispatch label, consecutive-error count,
   reversibility flags, in-flight calls — `internal/supervisionapi`,
   `internal/agent/runner`.
3. Renderer with budget, and the prompt asset — `internal/supervision`,
   `internal/prompts/supervision`.
4. `Batch` with the arbiter; rules routed through it; the lane upgrade and
   the fatal capture for supervised dispatches —
   `internal/agent/supervision`, `internal/agent/runner`,
   `internal/tools/delegate.go`.
5. `supervise` tool, tool doc, belt admission — `internal/tools`,
   `cmd/yaah`.
6. Envelope records — `internal/jobs`, `internal/agent/runner`,
   `internal/dispatch`.
7. Config and the headless decider — `internal/config/supervisor.go`,
   `cmd/yaah`.
8. yaah-1050.1.5.

## 7. Measurement notes for yaah-1050.1.5

- Headless mode, the v1 prompt, 5 s, `trigger: always`: the measured
  configuration.
- Report the worker model and the decider model. The paper's figures belong
  to one model pair.
- Solo, coop and supervised arms. Supervised and unsupervised runs differ
  only in `policy`.
- `require_reversible: true`, so every supervised worker is on an isolated
  lane and revert is available.

## 8. Acceptance mapping

- [x] Supervisor reads a complete per-agent effect stream without perturbing
      the worker.
- [x] `redirect` and `revert` operate on running sub-agents (.2, .3).
- [x] Batched cross-agent snapshot (.1.1).
- [ ] Decisions are a per-agent list from one call per tick → §4.5.
- [ ] The dispatching agent can run the loop itself → §4.7.
- [ ] `policy` defaults to `none` and today's behavior is unchanged → §4.9.
- [ ] Interventions reach the dispatch result → §4.10.
- [ ] Then yaah-1050.1.5.

## 9. Follow-ups and out of scope

- **Intent in the trace** (audit gap #1). In-flight calls are visible to the
  policy from §4.2. Writing a call's declaration to the trace before it
  returns is a follow-up: every trace consumer must then tolerate
  declarations without a result. Read why the pair is written together
  before changing it.
- **A `restrict` action.** Tighten the blocking guard for one scope at a
  tick (deny shell, or deny writes outside a path, for N ticks), enforced
  before execution with no model call in the hot path. Owner decision,
  2026-11-24: a follow-up, not part of this task.
- **A model gate on individual tool calls.** Possible through the approval
  middleware's pattern; not planned. It adds latency to every call and is
  not what the paper measured.
- **Container sessions.** A sub-agent there has no scope of its own, so
  there is nothing to roll back. Closing that means a worktree per sub-agent
  inside the container.
- **Ignored files** survive a restore on the git worktree backend.
- **Scope-handoff.** Deterministic in the reference code: when two agents'
  modified-file sets overlap, fork the leader's scope once. A candidate for
  the rules engine, not for this task.
- **Per-effect timestamps.**
- **Incremental cursor reads.** `on-change` uses `Through` only as a change
  signal.
- **A worker-to-worker relay** for the coop arm of yaah-1050.1.5.
- **Interactive interval and headless model choice.**
