# Dispatch retry / fallback / recover — adopt/decline decision

Status: **decided** — `yaah-hxk3.5` (spike `yaah-1163`)
Reference: `shepherd_runtime/combinators/{retry,gating}.py`

## Decision

Adopt a narrow, dispatch-boundary retry for the preflight and report lanes,
with total-timeout semantics and per-attempt evidence. Decline the generic
`fallback`/`recover` combinators, arbitrary `until` predicates, and the
Python-style authoring API. No runtime implementation is done here; the adopt
items are filed as follow-up beads.

## Reference semantics

Shepherd builds every combinator from four primitives — fork, merge, discard,
materialize. The relevant ones:

| Combinator | Behavior |
|---|---|
| `retry(task, max, until, delay, backoff)` | re-run in a fresh fork up to N; discard failed forks; merge the last attempt even when `until` still fails |
| `fallback(*tasks)` | try in order; first success wins; failed forks discarded |
| `recover(task, on_error)` | run once; on error discard the fork and return a default |
| `gate` / `budget` / `timeout` | conditional commit / resource reject / time reject |

The useful invariant: **a failed attempt leaves no retained effects**.

## yaah's current surface

- **Provider layer** — transient-error retry with backoff (`retry_backoff_secs`)
  and provider/model fallback (`FallbackConfig`).
- **Workflow DSL** — `retry.max_attempts` + `backoff_seconds`, `gate`/`assert`,
  and the scheduler's `timeout_seconds` backstop (`internal/workflow/exec`).
- **Dispatch boundary** — `ResultRecovery.Retryable` is a *signal*, not an
  action: `delegate` does not re-dispatch a failed sub-agent.

Retry exists at the provider and workflow layers; the dispatch boundary has no
automatic retry. That is the gap this decision closes.

## Adopt / decline

| Surface | Verdict |
|---|---|
| Dispatch retry — preflight + report lanes | **Adopt** |
| Total-timeout semantics (per attempt = remaining) | **Adopt** |
| Per-attempt evidence (`yaah.subagent.dispatch_attempt.v1`) | **Adopt** |
| `agents.subagent.retry` config + per-role override | **Adopt** |
| Generic `fallback` over roles/prompts/models | **Decline for v1** |
| Generic `recover` / default values | **Decline for v1** |
| Arbitrary `until` predicates / a predicate DSL | **Decline** |
| Authoring API (`task`/`step`/`sequence`/`branch`/`loop`) | **Decline** |
| Auto-retry of sealed proposals | **Defer** (after v1 evidence) |
| `race` / `parallel_all` / `speculate` | **Coordinate** with `yaah-95ri`, `yaah-9bq` |

## Lane rules

| Lane / phase | Policy |
|---|---|
| Preflight failure (daemon, image, bootstrap, provisioning) | retry with backoff; nothing ran |
| Report lane (worktree or container) | retry on timeout/provider/transient; world destroyed |
| Proposal lane before seal | retry only if provisioning failed before any work |
| Proposal lane after seal | no automatic retry — a sealed partial is reviewable and consume-once |
| Shared lane | no automatic retry — mutates the host directly |
| Parent cancellation / user halt | never retry |

## Budget semantics

- `timeout_seconds` is the **total** dispatch budget; each attempt gets the
  remaining budget.
- `max_iterations` / `max_turns` are per-attempt.
- Usage is aggregated across attempts; `max_elapsed_seconds` bounds backoff.

## Config / API shape

```yaml
agents:
  subagent:
    retry:
      max_attempts: 2        # 0 = off
      backoff_secs: 1
      max_elapsed_seconds: 30
      only: [preflight_transient, provider_transient, timeout, stuck]
    roles:
      developer:
        retry: { max_attempts: 1 }
```

Failure classes (only the first four are retry candidates):
`preflight_transient`, `provider_transient`, `timeout`, `stuck`, `content`,
`configuration`, `cancelled`, `sealed`.

## Evidence

Per-attempt `yaah.subagent.dispatch_attempt.v1` fact (attempt, status,
retryable, failure_class, scope_id, trace_owner_id, proposal_id); the final
envelope gains `attempts`, `retried`, and attempt IDs.

## Follow-up beads

- Adopt: dispatch-boundary retry for preflight/report lanes + `agents.subagent.retry`.
- Adopt: per-attempt `yaah.subagent.dispatch_attempt.v1` evidence + envelope `attempts`/`retried`.
- Coordinate: failure taxonomy with `internal/agent/errorclassify`.
- Defer: auto-retry of sealed proposals and `fallback`/`recover` policy after v1 evidence.
