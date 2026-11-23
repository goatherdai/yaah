# CooperBench parity audit: what the paper's gains actually depend on

Status: audit, 2026-11-23. Source: *Shepherd: Enabling Programmable
Meta-Agents via Reversible Agentic Execution Traces* (arXiv:2605.10913v3),
full text including Appendices A–F, read against the current yaah source and
`docs/goatherd-parity.md`. Owner bead: `yaah-1050` family (new epic filed from
this audit).

## Executive summary

The goal was for yaah to score similarly to Shepherd on benchmarks such as
CooperBench. The parity effort succeeded on the **record, world, and
authority** layers (exact ABI, worktree/container worlds, jails, egress,
sealed proposals, continuation/fork lineage), but the paper's headline
numbers come from a different layer: **non-perturbing effect-stream
observation plus live scope interventions**. That layer is where yaah is
partial or declined, and that is where the divergence that costs benchmark
performance lives.

Crucially, the CooperBench gain is **not primarily a revert/fork result**.
In the paper's own action mix, cheap `inject` (steer) dominates and `revert`
is rare (4.6–7.9% of interventions). The enabling primitive is that the
supervisor subscribes directly to *both workers' complete effect streams*,
sees intent before outcome, and polls cheaply every few seconds without
inflating worker context.

## 1. What the paper does and does not claim

Appendix A.1 is explicit: the three case studies are proofs of existence.
The authors state they do **not** claim optimality of their meta-agent
policies, robustness at scale, or "that the headline numbers cannot be
matched without Shepherd." A head-to-head burden of proof is out of scope.

Consequence: matching ~54.7% on CooperBench is an empirical target, not a
parity theorem. But the mechanism that produced it is identifiable, and
yaah should reproduce the mechanism before expecting the result.

Two other caveats from Appendix A matter:

- **Supervision/proposer cost**: on short tasks the meta-agent can cost more
  than the worker; the favourable regime depends on task length and the
  worker/meta-agent cost ratio.
- **Counterfactual replay assumes weak coupling**: if an edit touches a
  widely-propagating component (e.g. a tool's system prompt), the replay
  suffix is the whole trajectory and the cache buys nothing.

## 2. What actually drove 28.8% → 54.7% (§5.1, Appendix E)

| Item | Paper detail |
|---|---|
| Setup | 479 structurally-conflicting pairs, 25 repos, two Haiku 4.5 workers in separate OverlayFS sandboxes plus a shared relay; baseline `coop` 28.8%, solo ceiling 57.2% |
| Enabling primitive | Supervisor subscribes **directly to both effect streams**, observing completely and non-perturbingly. The paper attributes the gain to this: naive coop workers see only what peers *chose to send*, filtered through a flawed view |
| Actions | `inject`/steer (append guidance, preserve history and KV cache), `handoff`/redirect (abort session, fresh session on same identity, files kept), `discard`/revert (same + OverlayFS rolled back to pre-run snapshot). Cost order: steer ≪ redirect = revert |
| Action mix | Sonnet: steer 26.4%, redirect 18.7%, revert 7.9%. Opus: steer 39.2%, redirect 31.5%, revert 4.6%. Most ticks are `none` |
| Cadence | Batched snapshot every 5s, <3K tokens, trailing 25 tool calls (≈150 B each), modified-file set, consecutive-error count, last action and age. One meta-agent call per tick across the pair |
| Non-perturbation | Appendix C.5: worker context byte-identical with/without supervisor — 21 msgs / 1449 chars. Zero context inflation |
| Overhead | solo 28.4 min, coop 19.8 min, Sonnet 21.2 min (+1.4), Opus 24.2 min (+4.3) |

The other two mechanisms are different and should not be conflated with
CooperBench: §5.2 (CRO) leans on **byte-identical replay** and suffix-only
execution with KV-cache reuse (~95% hit from K=2); §5.3 (Tree-GRPO) leans on
**cheap exact forking** for per-step credit in a training loop.

## 3. Ranked yaah gap map for CooperBench

| # | Paper primitive | yaah today | Evidence | Impact |
|---|---|---|---|---|
| 1 | Typed immutable effect stream, intent/outcome separated, cursor, subscribe-to-commits | Best-effort `pubsub.Broker` (drops on full channel), no sequence numbers (explicitly declined), no subscribe-from-commit, no per-agent batched snapshot | `internal/pubsub/broker.go:68-82`, `internal/agent/events/events.go:29-42`; subagent audit 2026-11-23 | **Highest** — this is the property the paper credits for the result |
| 2 | `redirect`: abort live loop, fresh loop, same identity, files preserved, guidance as message 1 | No live equivalent. `proposal continue/fork` re-seeds a new loop from a **sealed** output, post-hoc, worktree-lane only | `internal/dispatch/retained.go:1316-1322` | High — 18.7–31.5% of paper interventions |
| 3 | `revert`: roll live workspace back to pre-run checkpoint, fresh loop | No live equivalent. Per-turn checkpoint is private, off by default, single-use, max 3, failure-triggered only; its interface says branching is out of scope | `internal/agent/turn_checkpoint.go:5-13`, `loop.go:464,489`, `task_runner.go:502-508` | Medium — 4.6–7.9% of interventions, but load-bearing for corrupt-write cases |
| 4 | LLM supervisor with four-action schema, 5s batched cadence, "default to none" | Automatic supervisor is deterministic rules, advisory, root-scope exempt; only manual `supervisor` tool reaches orchestrator. No paired-worker snapshot or policy prompt | `internal/agent/supervision/service.go:1-11,653-661`, `docs/supervision-parity.md:35-41` | High — the orchestrator could play this role but lacks feed and actions |
| 5 | `steer`: append guidance, preserve prefix and cache | **Present**: `supervisor inject` → `[STEER]` appended as a user message into a live loop | `internal/tools/supervisor.go:194-221`, `internal/agent/pipeline/steer.go:24-50` | Already covered — the cheap path works |
| 6 | `scope.fork()`: O(1) CoW, atomic agent+env, 134–143 ms, image-size-independent, byte-identical prefix for cache reuse | Forks at **dispatch boundaries**: worktree or sibling container, `ForkFromOutput` with baseline/seed lineage. Not mid-turn, not atomic over a live agent | `internal/dispatch/dispatch_container.go:264`, `retained.go:2084-2097`, `docs/goatherd-parity.md` D9/D13 | Medium for CooperBench; high for CRO/Tree-GRPO |
| 7 | Reversibility tiers with pre-materialization interception | Enforcement at tool/dispatch boundary (jail + settlement monitors), advisory `EffectBus`; per-effect handler and effect/context fold explicitly declined | `docs/supervision-parity.md:72-87`, `docs/context-effect-fold-decision.md:30-46` | Medium — removes the unified watched/refused/recorded channel |

## 4. Where parity effort went vs. where the gains live

`docs/goatherd-parity.md` scorecard and decision records show the emphasis:

- Shipped/exact: kernel ABI, retained runs + four settlement verbs, dispatch
  worlds, authority/grants/egress, retained outputs, continuation/fork,
  retained-result threading, janitor.
- Partial/declined: supervision (`yaah-hxk3.1` → Partial), task/combinators
  (DSL decision), contexts/effect fold (declined), citation checking
  (declined).
- D6/D7/D8 bypass Ring 2 `TaskControl`, materialize receipts, and retaining
  the orchestrator. D8 is **not** the gap: Shepherd's meta-agent is also
  unconfined. The gap is the surface exposed by retained **runs**, not the
  status of the driver.

So the work achieved the substrate's bookkeeping parity while declining the
substrate's control surface — which is precisely what the paper's
experiments exercise.

## 5. Figure 2 scenario coverage

Figure 2 is the results figure for the three applications, not three variants
of the supervision flow. They have very different support profiles.

| Figure 2 scenario | Verdict in yaah today | Owner |
|---|---|---|
| **A — Runtime supervision** (CooperBench) | **Partly** — `[STEER]` exists; complete observation and live redirect/revert do not | `yaah-1050.1` |
| **B — Counterfactual meta-optimization** (CRO) | **Partly, as a different system** — the proposer loop shape is buildable on the workflow DSL; the substrate properties that made CRO cheap are absent | `yaah-1050.2` |
| **C — Meta-agent-guided Tree-GRPO** | **No** — training loop, GRPO, sibling rollouts; a different product category | Out of scope |

### Scenario B in detail

The paper's CRO (§5.2, Appendix F) needs:

- typed per-example effect dumps and `read_effect_trace` /
  `grep_effect_traces` / `diff_traces` (F.2.3), so the proposer inspects prior
  runs at the model-call level rather than the metric level;
- a typed subtask cache keyed by `(source-hash, inputs-hash)` with a
  composite top-level key, so editing one subtask invalidates only its
  subtree (F.2.4);
- suffix-only replay from the first event whose dependencies the edit
  changes, with a byte-identical restored message prefix and KV-cache reuse;
- host-side reflection contract and improve/protect/invariant targeted-eval
  gates (F.2.5), so the proposer never self-grades;
- a candidate pool, ledger, dev promotion and final dev-based selection.

yaah has the workflow DSL (`delegate`, `fanout`, `branch`, `gate`, `select`,
`settle`; `workflow run/resume/replay`, assert nodes, store/trace/registry)
but **no effect-level introspection and no typed subtask cache**. `resume` is
hash-pinned and refuses a changed spec hash; finished runs are final.

Consequence: a CRO-like loop can be built today, but every candidate is a
**full re-run** rather than a counterfactual suffix replay. Attribution is
noisier and wall-clock much higher, so the 27–58% savings and the "hold
everything else fixed" property that the paper credits for the gains would
be absent. Without those, the result resembles GEPA/MetaHarness more than
CRO. This is tracked as epic `yaah-1050.2`.

### Other results

- **Trajectory compression (Appendix D)**: ~1/3 of compressions are `f=0`
  full restarts with a hint prepended — close to yaah's `continue/fork` with
  an instruction. Mid-trajectory fork-step cases are not reachable. **Partly
  reachable.**
- **Tree-GRPO (§5.3)**: requires a training loop (GRPO, 8×H100, 128 rollouts
  per step) and cheap forking at a meta-agent-chosen turn for sibling
  advantages. **Out of scope** for an inference-time assistant; the paper
  itself lists "post-training agents to use the substrate" as future work.

## 6. Recommendations, in impact order

1. **First-class per-sub-agent effect stream.** Typed intent/outcome records,
   cursor, and a compact batched supervisor snapshot (trailing N tool calls,
   modified files, error counts, last action/age). Verify zero worker context
   inflation. This is the prerequisite for everything else.
2. **Two live intervention tools.** `redirect` (abort loop, fresh loop with
   guidance as message 1, files preserved) and `revert` (roll workspace back
   to pre-run checkpoint, fresh loop). Keep `[STEER]` as the cheap path.
3. **Supervisor policy and cadence.** Expose the feed and the four-action
   schema to the orchestrator, or provide a separate supervisor LLM; default
   to `none`; batch across agents to keep cost flat in worker count.
4. **CRO prerequisites** (`yaah-1050.2`, separate larger increment): effect
   introspection tools, subtask cache keyed by source/inputs hashes, replay
   from a checkpoint with restored message prefix, reflection and
   targeted-eval gates.
5. **Explicitly record Tree-GRPO as out of scope** unless a training loop is
   adopted, so the scorecard does not imply it is attainable.

## 7. Acceptance / verification

- Do **not** claim a CooperBench number without running the benchmark. The
  paper's own A.1 disclaims uniqueness, so the target is empirical.
- Before measurement: (a) a supervisor can read a complete per-agent effect
  stream non-perturbingly; (b) `redirect` and `revert` are exposed as tools
  and operate on a running sub-agent; (c) worker context is byte-identical
  with and without the supervisor attached; (d) the action policy defaults to
  `none`.
- Then run the 479-pair structurally-conflicting CooperBench split with
  paired baselines (solo / coop / supervised) and report steer/redirect/revert
  coverage alongside pass rate and wall-clock, matching Appendix E's
  reporting.

## 8. References

Paper: §3.1–3.4 (tasks, effects, scopes, trace), §4 (fork/replay cost),
§5.1 + Appendix E (CooperBench protocol), §5.2 + Appendix F (CRO), §5.3 +
F.6/F.7 (Tree-GRPO), Appendix D (trajectory compression), Appendix A.1
(limitations), Appendix C.5 (observation overhead).

yaah: `docs/goatherd-parity.md` (scorecard, D6/D7/D8/D9/D13),
`docs/supervision-parity.md`, `docs/context-effect-fold-decision.md`,
`internal/pubsub/broker.go`, `internal/agent/events/events.go`,
`internal/agent/supervision/service.go`, `internal/tools/supervisor.go`,
`internal/agent/pipeline/steer.go`, `internal/agent/turn_checkpoint.go`,
`internal/dispatch/retained.go`.
