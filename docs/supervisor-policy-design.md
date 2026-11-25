# Supervisor policy design — yaah-1050.1.4

> Plan doc for the remaining implementation task of the CooperBench-parity
> control surface epic (`yaah-1050.1`). Target: reproduce Shepherd's
> four-action LLM-supervisor policy (arXiv:2605.10913v3, §5.1 + Appendix E) —
> the mechanism behind the 28.8% → 54.7% CooperBench gain.
>
> Companion reading: `docs/cooperbench-parity-audit.md` (gap #4),
> `docs/supervision-parity.md`, `docs/live-redirect-redesign.md`.

## 1. Target

Expose a **four-action schema** — `none` / `steer` / `redirect` / `revert` — to
a supervisor LLM that:

- runs a **batched cadence**: one meta-agent call every few seconds across all
  live sub-agents (not per agent), so supervisor cost is flat in worker count;
- **defaults to `none`**: the modal tick is a no-op; parse failures, config
  absence, and error paths all degrade to no intervention;
- keeps the existing `inject` → `[STEER]` user-message prefix as the cheap
  steer path (preserving the worker's KV cache);
- consumes a snapshot matching Appendix E's spec: trailing 25 tool calls,
  modified-file set, consecutive-error count, **last action and its age**,
  under a <3K-token budget.

Paper reference points for parity claims (used by yaah-1050.1.5):

- Action mix: steer 26–39%, redirect 19–32%, revert 5–8%, `none` dominant.
- Interventions per run: 2.6–6.5; latency overhead +1.4 to +4.3 min
  (model-dependent).
- One meta-agent call per tick across the **pair**, at most one intervention
  per tick.

## 2. Current state (post .1–.3, PR #398/#400)

| Piece | State | Evidence |
|---|---|---|
| Batched cross-agent snapshot, <3K tokens | Done — 50 agents / 25 trail calls / 12 files / 160B args/errors; 6000B render cap ≈ 1.5–2K tokens | `internal/supervisionapi/supervisionapi.go:80-155`, `internal/agent/supervision/snapshot.go:21-34`, `internal/tools/supervisor.go` (`snapshotRenderBudget = 6000`) |
| `steer` | Done — `registry.Inject` → `Run.SteerCh` → `SteerMiddleware` drains into `[STEER]` user msg before the next provider call | `internal/agent/pipeline/steer.go:24-50` |
| `redirect` | Done — generation CAS claim → `RedirectRequest{Guidance, Revert}` → adoption loop successor with files kept | `internal/agent/supervision/registry.go:299-348`, `internal/agent/runner/task_runner.go:818-826` |
| `revert` | Done — same handoff with `Revert: true` (workspace rollback to pre-run snapshot) | `RedirectRequest.Revert` |
| Atomic claim→consume→close seam | Done (PR #400) | `registry.go` `ConsumeRedirectOrClose` |
| Deterministic advisory rules (destructive_file, high_error_rate, stuck_detection) | Done; per-event on the EffectBus; root-scope exempt | `internal/agent/supervision/service.go` |

## 3. Gaps this task must close

1. **No decision layer.** Actions exist only as tool calls a human/orchestrator
   issues. Nothing chooses among the four actions on a cadence. (The workflow
   `judge:` gate is unrelated: `internal/workflow/exec/gate.go`.)
2. **No machine-readable `none`.** Refusals/results are free text; the schema
   needs an explicit verdict type where `none` is first-class.
3. **No cadence.** Rules are per-event; the snapshot is pull-on-demand. No
   timer-driven, once-per-tick, cross-agent batch.
4. **Snapshot missing "last action age".** It carries `LastAction` but no
   timestamp; per-action TIME is a declared gap (retained records carry no
   timestamps: `internal/supervisionapi/supervisionapi.go:66-74`). The policy
   needs age to distinguish "working" from "wedged".
5. **Cursor not consumed incrementally.** `Through` (owner ordinal) is
   reported, but every read is a full `0..through` prefix probe
   (`snapshot.go:44-52` doubling probe).
6. **Default-to-none posture.** The deterministic rules default ON when the
   config stanza is live (`internal/config/supervisor.go:108-147`). Fine as a
   guard rail, but the new *action policy* must default off/none, and the
   config surface must keep the two regimes explicit.

## 4. Design

### 4.1 Dedicated supervisor loop (decision)

A dedicated supervisor LLM loop lives beside the deterministic service in
`internal/agent/supervision`, rather than pushing the role onto the
orchestrator. Rationale: cost flat in worker count, zero root-context
perturbation, keeps root-scope exemption clean, matches Appendix E's
standalone meta-agent. An orchestrator-facing passive `supervisor decide`
sugar can come later.

### 4.2 Verdict schema

New typed verdict in `internal/supervisionapi` (neutral, shared by tools and
engine):

```go
type ActionKind string // "none" | "steer" | "redirect" | "revert"

type Verdict struct {
    Action   ActionKind
    ScopeID  string // target sub-agent scope
    Guidance string // steer/redirect payload
    Reason   string // bounded, for the audit trail
}
```

- One verdict per tick, at most one intervention (paper behavior).
- Any parse failure, empty response, or schema violation → `none`.
- `revert` rides the existing redirect handoff with `Revert: true`; it is not
  a fifth mechanic.

### 4.3 Loop cadence

- One tick per interval (default 5s — the Appendix E protocol; configurable
  for interactive cost).
- Per tick: build **one** paired snapshot across all live sub-agents
  (existing `Snapshotter`), make **one** LLM call, apply at most one verdict.
- The loop is read-only except through the existing registry seams — the
  worker's context and workspace are untouched by observation.

### 4.4 Application seams (reuse, do not reinvent)

| Verdict | Mechanism | Seam |
|---|---|---|
| `steer` | trace fact → `registry.Inject` → `SteerCh` → `[STEER]` msg | existing |
| `redirect` | `registry.Redirect` CAS (single winner) → `ConsumeRedirectOrClose` adoption | existing |
| `revert` | same as redirect with `Revert: true` | existing |
| `none` | no-op; record the decision for the audit trail | new (trivial) |

At-most-once semantics come free from the generation CAS. Root scope remains
exempt from all automatic actions.

### 4.5 Snapshot upgrade (prerequisite, small)

- **Timestamps for age**: stamp a write-path timestamp on retained trace facts
  so the snapshot renders `last action (Ns ago)`. Closes the declared gap at
  `supervisionapi.go:66-74`.
- **Incremental cursor**: consume `Through` across ticks so each read is
  bounded by what changed since the last tick. This also gives the policy a
  cheap "delta since last look" signal for free.

### 4.6 Policy prompt

- Snapshot (≤3K tokens) + a policy preamble with a strong **"prefer `none`"**
  prior: intervene only on concrete evidence of a wedge or a wrong-direction
  loop; steering is for small corrections, redirect for lost trajectories,
  revert for destructive or deeply wrong turns.
- Output constrained to the verdict JSON. No chain-of-thought requirement —
  a one-line `reason` field is the whole explanation surface.
- Prompt lives in `internal/prompts/` alongside the existing identity/role
  prompts.

### 4.7 Config

```yaml
supervisor:
  policy: none        # none (default) | llm — the action policy; rules stay separate
  policy_model: ""    # cheap model by default; explicit entry to override
  policy_interval: 5s # batched cadence
```

- `policy: none` (default): loop not started. Nothing changes for existing
  users — **acceptance (d): action policy defaults to none**.
- `policy: llm`: loop starts; every failure path degrades to `none`.
- Deterministic rules keep their own stanza and semantics; they are the
  guard rail, the policy is the coach. The two must not be conflated.

## 5. Testing / verification

- Verdict schema: parse-success, parse-failure→none, unknown-action→none.
- Loop: fake LLM returning scripted verdicts; assert steer/redirect/revert
  land via registry seams exactly once per tick; assert `none` leaves the
  registry untouched.
- Non-perturbation: snapshot+loop observation leaves worker context and
  workspace byte-identical (this doubles as the .5 paired-run precondition).
- Age rendering: a stamped fact renders with a plausible age; unstamped
  legacy facts render without one.
- Default posture: unconfigured engine starts no policy loop.

## 6. Sequencing

1. Verdict schema + parse-fail→none (`internal/supervisionapi`).
2. Timestamp stamp on trace facts + snapshot `age` rendering (+ incremental
   cursor consumption).
3. Policy prompt + config (`internal/config/supervisor.go` entries).
4. Supervisor loop wired to the existing seams, default-off.
5. Tests as in §5.

## 7. Open questions (non-blocking)

- **Interactive tick default**: 5s matches the benchmark protocol exactly;
  interactive sessions may prefer 15–30s to save supervisor cost. Config
  covers both; pick the bench value in `.5`'s run config either way.
- **Model choice for policy**: paper notes supervision cost as the main
  downside (Sonnet +1.4min / Opus +4.3min per run). Default to a cheap model;
  let `.5` measure the cost/quality tradeoff on the paired runs.
- **Per-tick delta feed**: with the incremental cursor, the prompt could show
  "since last tick" instead of the trailing window. Start with the Appendix E
  snapshot verbatim; measure before deviating.

## 8. Acceptance mapping

- [x] Supervisor reads a complete per-agent effect stream without perturbing
      the worker (done — snapshot is trace read-only by construction).
- [x] `redirect` / `revert` operate on running sub-agents (done — .2/.3).
- [ ] **Action policy defaults to `none`** → §4.7 config default.
- [ ] **Four-action schema + batched cadence** → §4.2–4.3.
- [ ] Then yaah-1050.1.5: 479-pair solo/coop/supervised runs — no scores
      claimed by analogy.
