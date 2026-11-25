# Glossary and synonyms

Plain definitions for the words used in the supervision docs
(`supervisor-policy-design.md`, `cooperbench-parity-audit.md`,
`supervision-parity.md`, `live-redirect-redesign.md`) and for the Shepherd
paper they are measured against.

How to read it:

- The bold word at the start of an entry is the one to use when writing.
- *Also* lists the other names the same thing goes by, in code, tools,
  config or the paper.
- *(planned)* marks something in the design that is not in the tree yet.
- Some words have two different meanings in this repo. They are in §9;
  check there before reusing one.
- Workflow words (proposal, settlement, skeleton, leaf) are defined in
  `workflow-dsl-plan.md` §17 and only summarized here.

Living document, started 2026-11-24 from the yaah-1050.1.4 design review.

## 1. Synonyms at a glance

| Concept | Use this | yaah code or tool | yaah config | Shepherd's name |
|---|---|---|---|---|
| The agent that started the workers | dispatching agent | the `delegate` caller | — | meta-agent |
| The LLM doing a task | sub-agent | `delegate`, `subagent_jobs` | `agents.subagent.*` | agent (paper), worker (code) |
| The non-LLM process | host | task runner, registry, supervision service | — | orchestrator (paper), runtime (code) |
| One sub-agent's work | dispatch | `dispatch_id` | — | agent |
| The handle supervision acts on | scope | `scope_id`, `list_scopes` | — | agent |
| One pass of the loop | tick | `Batch.Next` (planned) | `supervisor.interval` | tick |
| The per-tick state summary | snapshot | `snapshot` verb, `Snapshotter` | — | snapshot (paper), representation (code) |
| Add a message to a worker | steer | `inject` verb, `Registry.Inject` | `supervisor.actions` | inject (paper), steer (code) |
| Restart a worker with new instructions | redirect | `redirect` verb, `Registry.Redirect` | `supervisor.actions` | handoff (paper), redirect (code) |
| Roll back a worker, then restart | revert | `revert` verb | `supervisor.actions` | discard (paper), revert (code) |
| Cancel a run | stop | `halt` verb, rules engine | `supervisor.halt_root` | — |
| The LLM part that decides | policy | `Decider` (planned) | `supervisor.policy` | meta-agent |
| The model that decides | policy model | — | `supervisor.policy_model` | meta-agent model |
| The fixed-rule part | rules engine | supervision service | `supervisor.rules` | — |
| Which actions are allowed | mask | — | `supervisor.actions` | `available_actions` |
| Interventions allowed per batch | cap | — | `supervisor.max_interventions` | `max_interventions` |
| Ticks since the last intervention | age | `Batch` state (planned) | — | age (paper), `ticks_since_last_action` (code) |
| A dispatch's result | envelope | what `delegate` returns | — | — |

## 2. Who is who

**Agent** — one LLM run with a conversation, tools and a trace. Either the
root agent or a sub-agent.

**Root agent** — the agent that owns a session: it talks to the human and may
start sub-agents. *Also:* session root, the orchestrator (in yaah docs; see
§9), root scope (its supervision handle, used loosely).

**Sub-agent** — an agent started by a `delegate` call. It has its own
conversation, tools, role and usually its own workspace copy, and returns one
result to the agent that started it. *Also:* subagent (in config keys and
`subagent_jobs`), child, worker.

**Dispatching agent** — the agent that issued the `delegate` calls for a given
set of sub-agents. It is the root agent, or a sub-agent that dispatches
further. Supervision docs use this term because it is the agent that
supervises. *Also:* parent, orchestrator (yaah docs), meta-agent (Shepherd).

**Worker** — Shepherd's word for a sub-agent that is running its task under
supervision. It is the same thing as a sub-agent; "worker" only stresses that
it does the task while something else watches. *Also:* agent (Shepherd).

**Meta-agent** — Shepherd's name for the LLM that watches the workers and
decides whether to intervene. yaah has no separate meta-agent. In agent mode
the dispatching agent is the meta-agent; in headless mode the policy model
is. *Also:* the policy.

**Host** — the yaah process and everything in it that is not an LLM: task
runner, registry, supervision service, tools. The host acts only by fixed
rules. *Also:* the process, runtime, orchestrator (Shepherd paper), kernel
(the goatherd library underneath).

**Human operator** — the person at the keyboard. Writes config and specs,
reads reports and diffs, and is never inside the loop. Humans do not issue
tool calls.

**Role** — a named sub-agent definition in `.agents/roles/` or
`~/.agents/roles/`: its prompt, tools, output contract and budget. A role is
`primary` (may run a session and dispatch) or `subagent` (delegate-only), and
returns a proposal (the default) or a report (`output: report`).

## 3. Units of work

**Session** — one run of the root agent, with its trace and its root scope.

**Dispatch** — one `delegate` call together with the sub-agent run it starts.
Tracked and returned as one unit. *Also:* job (a background dispatch, in
`subagent_jobs`), child, run.

**Scope** — the handle under which one agent is supervised. A session has one
root scope; every dispatch has its own. Scopes are what the snapshot lists
and what decisions name. A scope outlives a redirect: the restarted run is a
new generation of the same scope. *Also:* run, agent (Shepherd counts "per
agent").

**Root scope** — the scope of the root agent. Supervision never targets it: it
cannot be redirected or reverted, and stopping it needs the
`supervisor.halt_root` opt-in.

**Generation** — one run of a scope. A redirect or revert ends the current
generation and starts the next. Redirect claims are per generation, so a
claim made against the old run cannot hit the new one.

**Batch** *(planned)* — the set of scopes supervised together by one caller.
One snapshot per batch per tick. The batch is *settled* when every scope in
it has finished.

**Lane** — the result contract of a dispatch. `proposal` returns a sealed set
of changes to settle. `report` returns text and nothing to settle. `shared`
runs directly on the workspace. An *isolated lane* is any dispatch with its
own workspace copy: proposal, or report that can write. *Also:* retained lane
and direct lane (older words in `sub-agents.md` for isolated and shared).

**World** — where a dispatch runs: a `git worktree`, a `container`, or the
shared host tree. Named in the envelope as `world`.

**Proposal** — a sealed, reviewable unit of proposal-lane work, settled
exactly once with `select`, `apply`, `release` or `discard`. See
`workflow-dsl-plan.md` §17.

**Envelope** — the typed result every dispatch returns, whatever its lane:
`status`, `dispatch_id`, `role`, `lane`, `world`, `model`, `duration_ms`,
`stop_reason`, `result`, plus lane-specific and failure fields. The design
adds intervention records to it. *Also:* dispatch envelope, result envelope.

## 4. The loop

**Supervision** — watching running sub-agents and intervening in them. It has
two halves: the rules engine (fixed rules, no LLM, on by default) and the
policy (an LLM, off by default).

**Tick** — one pass of the supervision loop: apply the pending decisions,
wait, collect what happened, return a snapshot. One `Batch.Next` call.
*Also:* cycle.

**Interval** — the time between ticks. `supervisor.interval`, default 5s. A
tick with no live worker is skipped.

**Trigger** — when a tick actually asks the model: `always` (every tick with
a live worker) or `on-change` (only when something moved).

**Snapshot** — the per-tick text summary of every scope in a batch: what each
worker is doing, what it has changed, and how long since anyone intervened.
Rendered under a token budget (default 3K). Not the workspace copy that
revert restores (see §9). *Also:* representation (Shepherd code), the
`snapshot` verb.

**Trail** — the last 25 tool calls of a scope, about 150 bytes each, with
long arguments cut.

**In-flight call** *(planned)* — a tool call that has started and not
returned. Held in memory by the batch and removed when the call returns.

**Effect stream** — the host's record of what a run does: tool calls,
results, file changes. Supervision reads it without touching the worker.
*Also:* trace, effect events, `EffectBus`.

**Through cursor** — how far along the effect stream a snapshot has read.
The field is `Through`. Reading starts again from the beginning each time (a
prefix probe).

**Decision** — one row of the model's reply: a scope, an action, a reason of
at most 20 words and guidance of at most 60 words. At most one per scope per
tick; unlisted scopes mean `none`.

**Action** — what a decision asks for: `steer`, `redirect` or `revert`.
Stopping a run is not a policy action.

**Guidance** — the text a decision sends to the worker. Required for every
action.

**Intervention** — a steer, redirect or revert delivered to a worker, whether
by the policy, the rules engine or the root agent by hand. A decision the
arbiter dropped is recorded but is not an intervention.

**Intervention record** *(planned)* — what is written down for each decision:
time, tick, scope, who decided (`policy`, `rules` or `root`), action, reason,
guidance, outcome, generation. Appended to the trace and carried in the
envelope.

**Age** *(planned)* — for a scope, the number of ticks since the batch last
intervened in it. Shown as "last action, 3 ticks ago". *Also:*
`ticks_since_last_action` (Shepherd code).

**Cooldown** — the minimum number of ticks between interventions on one
scope. `supervisor.cooldown_ticks`, default 1.

**Mask** — the actions the policy may use. `supervisor.actions`, default
steer, redirect and revert. *Also:* `available_actions` (Shepherd).

**Cap** — the most interventions one batch may make.
`supervisor.max_interventions`; 0 means no limit.

**Arbiter** *(planned)* — the single gate every intervention passes through,
whoever it comes from. It checks the target, the mask, the cooldown, the
action's preconditions and the cap, with the precedence
stop > revert > redirect > steer.

**Outcome** *(planned)* — what the arbiter did with a decision: `applied`,
`claimed` (accepted; takes effect when the worker's current call returns) or
`dropped`, with a reason.

**Settled** — every scope in the batch has finished. `supervise` with
`wait: settled` returns then.

## 5. Actions on a worker

**Steer** — add one message to the worker's conversation. History and files
are untouched. The cheapest action. *Also:* inject (the `supervisor` verb,
and Shepherd's word), `[STEER]` message, `Registry.Inject`.

**Redirect** — end the worker's run and start a fresh one on the same scope
with new instructions. Files stay as they are. *Also:* handoff (Shepherd's
word; also yaah code for the step where the runner picks the claim up),
`Registry.Redirect`.

**Revert** — a redirect that first rolls the workspace back to the pre-run
capture. *Also:* discard (Shepherd's word; do not use it in yaah, see §9).

**Stop** — cancel a run. The rules engine can stop; the policy cannot.
*Also:* `halt` (the `supervisor` verb), kernel halt.

**`halt_root`** — the `supervisor.halt_root` opt-in that lets a rule or the
tool stop the root agent's own loop.

**Restrict** *(follow-up, yaah-b1ko)* — a possible fourth action: narrow what
a worker may do for a while without ending its run. Not in the current
design.

**Scope-handoff** — Shepherd's move when two workers edit the same files:
copy the leading worker's whole scope to a new agent. In the reference code
it is a fixed workflow with no LLM. In yaah it is a candidate for the rules
engine, not a policy action.

## 6. Who decides

**Policy** — the LLM half of supervision: the part that reads a snapshot and
returns decisions. `supervisor.policy` says who does it: `none`, `agent` or
`headless`. *Also:* the meta-agent, "LLM supervisor" (in the parity audit).

**Policy model** — the model that decides in headless mode.
`supervisor.policy_model`. *Also:* decider model, meta-agent model.

**Decider** *(planned)* — the Go interface for "snapshot in, decision list
out". Only headless mode implements it.

**Headless decider** *(planned)* — the one implementation: a single
completion call per tick, with the paper's v1 prompt as system prompt and the
snapshot as user message. No conversation, no tools, no memory.

**Agent mode** *(planned)* — `policy: agent`. The dispatching agent runs the
loop itself through the `supervise` tool, and its own model writes the
decisions.

**Headless mode** *(planned)* — `policy: headless`. The host runs the loop
and calls the policy model once per tick. Used for measurement and workflow
nodes.

**`policy: none`** *(planned key)* — the default. No `supervise` tool, no
batch, no model calls. The rules engine still runs. This is today's behavior.

**Rules engine** — the non-LLM half: it watches effect events and fires fixed
rules (destructive tool, high error rate, stuck run) that steer or stop a
run. On by default whenever tracing is on, even with no `supervisor:`
stanza. *Also:* advisory rules, supervision service, `supervisor.rules`.

**`supervisor` tool** — the existing root-only tool with seven verbs:
`list_scopes`, `status`, `inject`, `halt`, `redirect`, `revert`, `snapshot`.
For one-off use by the root agent. Its interventions go through the arbiter.

**`supervise` tool** *(planned)* — the loop tool for agent mode. It binds to
the caller's running sub-agents, applies the decisions for the last snapshot,
waits one tick or until settled, and returns the next snapshot and the
results.

**Judge** — not a supervision word. A judge is the model verdict a workflow
`gate` or `select` node dispatches, in a fixed verdict shape. The design cites
it only as the precedent for a one-shot model call made by the host. In
Shepherd, `JudgePredicate` is a plain predicate with no model.

## 7. Making revert safe

**Pre-run capture** — the copy of a workspace taken before a run starts,
which revert restores. *Also:* pre-run snapshot (Shepherd; avoid, see §9),
baseline.

**Reversible** — a per-scope flag: a revert will really roll back. Fixed when
the dispatch starts. *Shipped in yaah-k0x1; the flag is set only where the
scope owns an isolated workspace the dispatch can restore.*

**`require_reversible`** — a config switch. When on, a supervised
dispatch that cannot roll back fails at start instead of running. On for
measurement, off for interactive use. *Shipped in yaah-ugtl, together with the
lane upgrade below; it only ever narrows a supervised dispatch.*

**Lane upgrade** — under supervision, a dispatch that would run
on the shared workspace is moved to the report lane so that it gets its own
workspace copy. *Shipped in yaah-ugtl. A guard rather than the everyday
mechanism: role routing already isolates a workspace-mutating dispatch, so the
upgrade fires for a lane source that is not the role registry (a plugin or
hand-built resolver) with a retained runtime present — the nested seam is not
one: a nested dispatch has no retained runtime and is refused under
`require_reversible` instead. See `supervisedLane`.*

**Redirectable** — a per-scope flag: false for the root scope,
true otherwise. *Shipped in yaah-k0x1.*

**Guard** — a check that can refuse a tool call before it runs, such as the
blocking shell guard or an approval. Refusals appear in the envelope. A guard
stops a bad call up front; the policy can only react to what has happened.

## 8. Measurement words (from the paper)

**Pair** — one CooperBench item: two tasks on one repository whose reference
patches conflict when applied independently. The paper's split has 479 pairs
over 25 repositories.

**Arm** — one of the three setups the paper compares: `solo` (workers
alone), `coop` (workers plus a relay), `supervised` (coop plus a
meta-agent).

**Relay** — the message channel between workers in the coop and supervised
arms. yaah does not have one yet (yaah-cny5).

**Coverage** — the share of pairs in which the meta-agent used a given action
at least once. Reported per model; the numbers do not add up to 100%.

**Mechanism parity** — having the same moving parts as the paper: tick,
snapshot fields, actions, cooldown and prompt. It is distinct from matching
its scores, which needs the same workers and model (yaah-1050.1.5).

**Overlay scope** — Shepherd's per-agent filesystem overlay (OverlayFS),
which revert rolls back. yaah uses worktrees instead.

## 9. Words with two meanings

- **Snapshot.** (a) The per-tick text summary of a batch. (b) The workspace
  copy taken before a run. (c) containerd's filesystem snapshotter plugin,
  set by `workspace.containerd.snapshotter`. Use *snapshot* for (a) and
  *pre-run capture* for (b). The paper says "pre-run snapshot" for (b).
- **Supervisor.** (a) The `supervisor` tool. (b) The `supervisor:` config
  section, which holds both the rules and the policy. (c) The supervision
  service. (d) In the paper, the whole system. Use the word only for the tool
  and the config key; say *rules engine* and *policy* for the parts.
- **`supervise` and `supervisor`.** Two tools. `supervise` (planned) is the
  loop tool for agent mode. `supervisor` (existing) is the seven-verb tool
  for one-off use.
- **Orchestrator.** In yaah docs, the root agent: the one that settles
  proposals and commits. In the Shepherd paper, the non-LLM host. The
  supervision docs avoid the word: say *root agent* or *host*.
- **Headless.** (a) Headless supervision mode, where the host calls the
  policy model. (b) A headless session (MCP serve, ACP), which has no
  interactive UI. The two are unrelated.
- **Policy.** (a) The supervision policy. (b) A `select` node's
  `policy: first_success | smallest_diff`, a fixed choice rule with no model.
  (c) A permission policy. Outside supervision docs, say *supervision policy*.
- **Discard.** In yaah, a settlement verb: throw away a sealed proposal. In
  the paper, revert a running agent. Never use *discard* for revert.
- **Fork.** In yaah, `proposal fork` re-runs a proposal's captured work in a
  new workspace and leaves the original open. In Shepherd, a scope fork
  copies a live agent's whole scope mid-run. Different operations.
- **Stop and halt.** The act is a *stop*. The tool verb is `halt`. Neither is
  a policy action.
- **Lane.** Proposal, report or shared. In `sub-agents.md`, *retained lane*
  and *direct lane* name the isolated-versus-shared distinction, not extra
  lanes.
- **Judge.** In yaah, a workflow verdict call. In Shepherd, a deterministic
  predicate.
- **Inject.** The `supervisor` verb. The act is a *steer*. Injecting into the
  root scope steers the root agent itself.

## 10. Where the other word lists are

- `workflow-dsl-plan.md` §17: proposal, settlement, lane, world, skeleton,
  leaf, spec hash, disposition.
- `sub-agents.md` and `agent-instructions.md`: the sub-agent and `supervisor`
  tool vocabulary as the model sees it.
