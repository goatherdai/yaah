# Shepherd ↔ yaah supervision parity

How yaah's supervision surface maps onto Shepherd's — what is adopted,
what is declined, and what remains open. "Shepherd" here means the
Python v0.3.1 runtime cloned at `.scratch/shepherd/` (commit `d34d5ca`).
The port path is Shepherd → goatherd-kernel/runtime → yaah; yaah's
supervision surface is a thin decoration over the imported kernel
machinery.

## Shepherd's supervision is three facilities, not one

1. **Gating combinators** (`shepherd_runtime/combinators/{gating,types}.py`)
   — `gate`/`budget`/`timeout` wrappers that bound a step's execution.
2. **Permission plans** (`effects/policy.py`, `dialect/permission_plan.py`)
   — `Match`/`Plan` effect surfaces plus monitor assignment.
3. **Effects supervisor handler** (`effects/` supervisor handler) — an
   unrelated per-effect hook, distinct from the permission-plan system.

`JudgePredicate = Callable[[Any, "EffectStreamLike"], bool]` is a
deterministic predicate, not an LLM call.

## Mapping

| Shepherd | yaah | Status |
| --- | --- | --- |
| Permission-plan `Match`/`Plan` | `internal/effectpolicy` (`Match`, `Plan`, `Conforms`) | Adopted (Stage 1) |
| Monitor assignment / `PermissionPlan` | `internal/permissionplan` (`Authority` → `Compile` → monitors + `Digest`) | Adopted (Stage 1) |
| Jail monitor | `internal/dispatch/dispatch_authority.go` — grants/network/containment lowered from the plan; digest recorded as `yaah.subagent.authority.v1` | Adopted (Stage 2) |
| Settlement monitor | `internal/dispatch/git_delta.go` (container manifest) and `internal/dispatch/proposal_merge.go` (worktree three-way merge) — delta posture declared in the plan, digest cited at seal and settle as `authority_plan_digest` | Adopted (Stage 3) |
| `gate` deterministic predicate (`JudgePredicate`) | `internal/workflow/assert` (named deterministic checks) + `internal/workflow/exec/gate.go` | Adopted — the `judge:` (LLM verdict) half is a yaah extension beyond Shepherd's deterministic predicate |
| `budget`/`timeout` combinators | not present in `internal/workflow/exec` | Not ported (`yaah-hxk3.5`) |
| Citation checking | not ported | Declined (`yaah-hxk3.2`) |
| Effects supervisor handler | advisory rules engine (`destructive_tool`, `high_error_rate`, `stuck_detection`) | Diverged — see below |

## Advisory-rules divergence

Shepherd's effects supervisor handler is a per-effect hook. yaah's
advisory rules are a scope-based `EffectBus` engine with three rules
(destructive tool, high error rate, stuck detection); the root scope is
exempt by policy, and only the manual `supervisor` tool reaches the
orchestrator. This is a deliberate divergence, not a port gap.

## Settlement posture

`authorityForDispatch` declares `DeltaApply`: yaah's `select`/`apply`
write the manifest's post-images directly, and the orchestrator invoking
the verb is the review. `DeltaReview` is declined as a distinct posture
because yaah has no review-gate config knob.

Both settlement lanes now cite the plan digest. The container lane persists
it in `meta.json` and records `authority_plan_digest` in its sealed/settled
facts; the worktree lane seals it in the proposal's durable metadata
(`retainedProposalMeta`) and records it in the three-way apply's settle
fact, so a rehydrated worktree proposal still cites the declaration it was
settled against.

The two lanes compile one declaration except for containment, because they
confine in different places. The container world *is* the jail — it shapes
mounts and a netns before the body runs — so `authorityForDispatch` claims
`ContainmentJail` under `auto`/`paddock`. The worktree world is host paths
under a git worktree and establishes no jail of its own (a declared grant or
egress posture is enforced later, by the runner's tool-layer confinement,
which the dispatcher does not provision), so
`worktreeAuthorityForDispatch` declares `ContainmentAdvisory` with
`FallbackEnforce` — advisory because no jail monitor exists for that world,
enforce so a declaration's clauses still compile instead of being refused.
The consequence is pinned by test: for `containment: off/none/advisory` the
two lanes produce the same digest; for `auto`, the empty default, and
`paddock`/`jail` they differ, and they must, because for that config the two
worlds enforce different declarations.

## Open parity

`docs/goatherd-parity.md` is the ledger of record. Open items: `yaah-hxk3.1`
(supervision, Partial — the biggest gap), `hxk3.2` (citation checking, not
ported), `hxk3.4` (contexts/effect fold, Partial), `hxk3.5`
(task/step/combinators — the workflow DSL exists under `internal/workflow/`
with `gate`/`assert` adopted; `budget`/`timeout` remain unported),
`hxk3.3` (scorecard, ongoing); umbrella `yaah-1tkp`.
