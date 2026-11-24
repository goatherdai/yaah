# Live redirect, redesigned as an in-dispatch loop handoff

- **Bead:** yaah-8gxg (P1, BUG) — blocks yaah-1050.1.2 (live redirect) and yaah-1050.1.3 (live revert)
- **Parent epic:** yaah-1050.1 — CooperBench-parity control surface
- **Audit:** `docs/cooperbench-parity-audit.md` gap #2 ("redirect: abort live loop, fresh loop...") — currently "No live equivalent"
- **Status:** design / analysis
- **Dependency:** `github.com/goatherdai/goatherd-runtime` **v0.26.0 → v0.27.0**

## 1. Problem

PR #393 implemented yaah-1050.1.2 (live redirect) with a relaunch hook that ran
the replacement loop **synchronously in the supervisor's goroutine** while the
parent dispatch still owned the scope, the post-run pipeline, and the result
envelope. Qodo review found 9 findings (1 High). The PR was closed and the
branch reset to main; this document is the redesign requirement.

**Framing note (important):** PR #393's code is **not in the tree**. Exhaustive
grep finds no `executeRedirect`, `Registry.Redirect`, `freshCtx`,
`UnregisterGeneration`, production `newLoop`, or `redirect` supervisor action.
The 9 findings are therefore **design constraints — what not to repeat** — and
map to *anchor sites* in the current code (the mechanisms the PR hooked). The
current tree already contains a **latent form of finding #4** (the
`Unregister` / `close(quiesce)` ordering) even with no redirect code present.

## 2. Current architecture (the seam)

`makeTaskRunner` (`internal/agent/runner/task_runner.go:322`) is **one dispatch
= one of everything**:

| Concern | Site | Today |
|---|---|---|
| Scope | `:445` `createSubAgentScope`; `:459-464` `defer mgr.Discard(subScope.ID())` | Lifetime bound to the parent closure |
| steerCh | `:483` `make(chan, SteerBuffer=8)` | One per closure (root session reuses one across loops — `cmd/yaah/wiring_stages.go:1019`) |
| Quiesce/Done | `:484`; `defer close` `:509` | Closed at closure unwind |
| Registration | `:486-507` `runs.Register(...)`; explicit `Unregister` `:604` | `:604` fires **before** deferred `close(quiesce)` `:509` — **latent finding #4** |
| Loop + Run | `:524` `NewSubAgentLoop`; `:570` `subLoop.Run` | Exactly one Run, one result `:674` |
| **Accounting gauntlet** | `:574-664` | see below |

**The single accounting seam is `task_runner.go:570-673`:**

```
570  result, runErr := subLoop.Run(ctx, prompt)
574    CaptureSubAgentTranscript        (WithoutCancel)
577    CaptureSubAgentStop
589    CaptureSubAgentDenials
590    recordSubAgentUsage              (WithoutCancel, same subTraceID)
594    recordConfinementDegradedFact
598    CaptureSubAgentConfinement
604    runs.Unregister(subScope.ID())   <- "the real cut"
618    empty-output gate               -> sets runErr
627    unexecuted-markup gate          -> sets runErr
636    fabricated-transcript gate      -> sets runErr
645    subagentTraceProfile (on error)
664    tools.AddSubAgentUsage
666    span attributes
674  return result, runErr
```

The gates **never early-return**, so usage is recorded even for rejected runs.
Skipping this block is exactly finding #9. Two independent usage channels exist
and a sequence needs **both per run**: the per-dispatch trace fact
(`recordSubAgentUsage`, `usage.go:20`) and the parent accumulator
(`AddSubAgentUsage` → `finishSubAgent`, `agent_dispatch.go:511-521`,
`finishOnce`-guarded).

The registry (`internal/agent/supervision/registry.go`) is a **generation-less
single-mutex map** `scopeID → *run`: `Register` overwrites `:139`, `Cancel`
acts on whatever occupant is there `:234`. No `Redirect`, no generation token —
the root of findings #3, #6, #7. The supervisor tool
(`internal/tools/supervisor.go:114`) has only
`list_scopes/inject/halt/status/snapshot`; `halt` is the sole blocking action
(Cancel + bounded 10s quiesce wait `:251-262`, then `scope.Halt`).

The effect stream (yaah-1050.1.1) records per-tool Declaration(intent)/
Capture(outcome) facts (`internal/agent/pipeline/trace.go`) chained by
`CausalParents`; the snapshot cursor is the kernel per-owner ordinal
(`AgentSnapshot.Through`, `supervisionapi.go:105-110`), deliberately not broker
sequence numbers (`events.go:36-43`). The `snapshot` action is a read-only join
of registry + trace prefix (`supervision/snapshot.go`).

## 3. The nine findings → anchors

| # | Flaw (PR #393) | Anchor site in tree |
|---|---|---|
| 1 | Result/stop/transcript/usage built from the **cancelled** original | Single `subLoop.Run` result `:570` → `:674` |
| 2 | Proceed after quiesce **timeout** with old `Done` open → two loops on one workspace | Quiesce-timeout select `tools/supervisor.go:251-262` (reporting-only today; the PR used it to *proceed*) |
| 3 | Redirect locks the first-observed registration, invokes the hook on the post-wait occupant | No generation; `Register` overwrites `registry.go:139`, `Cancel` `:234` |
| 4 | `Unregister` before `close(quiesce)`; wait-then-act sees `ErrNoRun` | **Latent today:** explicit `Unregister :604` precedes deferred `close(quiesce) :509` |
| 5 | Deferred `mgr.Discard` tears the scope down under a live replacement | `:459-464` deferred on the parent closure |
| 6 | Replacement reuses the old `steerCh` → cross-conversation leak | `:483` per-closure channel |
| 7 | Each hook returns its own `.Run`; a 2nd redirect strands the 1st caller | No result channel between generations; `Cancel` returns only `Done` |
| 8 | `freshCtx = WithoutCancel(ctx)` strips cancel/deadline/interrupts | `WithoutCancel` is a *post-run-write* idiom only (`:571-573`), never the run's ctx |
| 9 | Hook skips usage/transcript/gates/trace-profile | The `:574-664` gauntlet |

## 4. Target design — in-dispatch loop handoff

The dispatch loop owns a **sequence of runs on one scope identity**. A redirect
request sets *pending guidance* + a fresh loop for the parent loop to adopt when
the current run returns, so the parent owns every run's accounting and emits
**one** final result. The supervisor tool requests the redirect and **returns
promptly**.

1. **Reshape `makeTaskRunner` into outer-sequence + inner-run.**
   Outer (once per dispatch): create scope, steerCh, quiesce, register the
   bridge; the deferred `mgr.Discard` / `close(quiesce)` / `Unregister` move
   **here** and fire once at the end of the sequence. Inner (per run):
   `newLoop` on the **same scope + steerCh**, run it, and execute the **full
   `:574-664` gauntlet** — but **no** Unregister/Discard between runs. **Return
   only the final run's result** as the one tool result (#1, #7); usage
   accumulates across all runs (#9).

2. **Redirect is a pending request, not an immediate hook** (#2, #3, #8).
   The supervisor tool's new `redirect` action records *pending guidance + a
   fresh-loop request* against the scope identity **under the registry lock**
   and **returns promptly** — no blocking through the replacement, no
   `WithoutCancel` run context. The current run is cancelled; when
   `subLoop.Run` returns, the parent loop checks for a pending redirect and, if
   present, builds a fresh loop (guidance = first user message) and runs it.
   Loop until no pending redirect, then emit the final result.

3. **Generation binding** (#3, #6, #7). Each run carries a generation token. A
   redirect binds to the generation it stops. Add a generation-checked
   `UnregisterGeneration` so an old loop's run-end Unregister cannot delete the
   successor's registration, and `Redirect` re-resolves the *current*
   registration under the lock (bail `ErrNoRun` if it changed). Each generation
   gets a **fresh steerCh** (drain the old).

4. **Quiesce is a property of the sequence** (#2, #4). Close `Done` once after
   the *final* run, after the last Unregister. "Confirmed quiesce" for redirect
   = *the current loop's `Run` returns*, never a timeout-proceed. Fix the latent
   `:604`-before-`:509` ordering as part of this.

5. **Cancellable replacement** (#8). The replacement loop inherits the run's
   cancellable `haltCtx` (`WithCancel`, `:485`), not `WithoutCancel` — so halt,
   deadline, and session interrupts all reach it. Housekeeping writes keep
   `WithoutCancel` (its correct use).

6. **Scope/sandbox reuse** (#5). Same scope, workdir, sandbox across runs
   (design intent: "files already written stay on disk"). `mgr.Discard` must not
   fire between runs.

7. **Preserve the accounting gauntlet + effect stream** (#9). Run the full block
   per run under the **same `subTraceID`/trace owner** so the `Through` cursor
   stays monotonic and the yaah-1050.1.1 snapshot covers every run; re-`Register`
   per run so `snapshot`/inject/halt reach the active loop. The integrity gates
   gate *each* run (a fabricated report from run 2 must not pass).

## 5. goatherd-runtime v0.27.0

`v0.26.0 → v0.27.0` is one fix: **"don't record a fork that fails
registration"** (`scope.go`, +137/−25, `ghost_fork_test.go`). `forkWithBaseline`
now appends the `scope.forked` fact **under the same lock** that rechecks parent
state and registers the child — so a parent that changed during baseline capture
fails *before* any durable fork record exists, and `RecoverScopes` cannot
resurrect a ghost child (bead `shepherd-runtime-go-2jd`).

This is load-bearing for both redesigns:

- **yaah-1050.1.2 (redirect):** the handoff reuses/forks scope identity across
  runs; the atomic fork fact prevents ghost scope records when a run is replaced
  mid-flight.
- **yaah-1050.1.3 (revert):** revert = roll back to the pre-run snapshot — that
  checkpoint *is* the `forkWithBaseline` baseline/`ForkPoint` this commit makes
  atomic. The revert substrate depends on it.

**Action:** bump `go.mod` to `goatherd-runtime v0.27.0` and build the scope
lifecycle on its semantics.

## 6. Implementation breakdown (children of yaah-8gxg)

| Bead | Scope | Fixes |
|---|---|---|
| yaah-8gxg.1 | Bump `goatherd-runtime` → v0.27.0; align scope/fork lifecycle to its atomic fork semantics | (foundation) |
| yaah-8gxg.2 | Registry run generations: generation token, `UnregisterGeneration`, compare-and-swap `Redirect` | #3, #6, #7 primitives |
| yaah-8gxg.3 | Reshape `makeTaskRunner` outer-sequence/inner-run; hoist scope/steerCh/quiesce; per-run gauntlet; one final result; fix latent ordering | #1, #4, #5, #9 |
| yaah-8gxg.4 | Supervisor `redirect` action + pending-adoption loop (prompt return, cancellable replacement, fresh steerCh per generation) | #2, #8 |
| yaah-8gxg.5 | Regression tests for all 9 findings + full gates | acceptance |

Suggested order: **1 → 2 → (3 ∥ 4) → 5** (2 is a prerequisite for 3 and 4).

## 7. Regression test plan (one per finding)

1. Final envelope carries the **corrected** run's result, not the cancelled original.
2. Redirect does **not** proceed while the old `Done` is open (never two loops on one workspace).
3. Two concurrent redirects cannot double-launch (generation CAS → second gets `ErrNoRun`).
4. Fast-stop path still permits redirect (Unregister/quiesce ordering corrected).
5. `mgr.Discard` never discards a scope under a live replacement.
6. Fresh steerCh per generation; old buffered guidance does not leak into the new conversation.
7. A second redirect returns the correct final report, not a stale/cancelled one.
8. Supervisor `redirect` returns promptly; the replacement is cancellable via halt + session-interrupt + deadline.
9. Each run records usage/transcript/denials; empty / unexecuted-markup / fabricated reports from **any** run are rejected.

Plus: a **goatherd-runtime v0.27.0** bump test, and "PR #393's
synchronous-supervisor-goroutine approach is not reused" as a stated constraint.

## 8. References

- PR #393 (closed) — original flawed implementation; Qodo review comment 7113600638
- `docs/cooperbench-parity-audit.md` gap #2 (redirect) and #3 (revert)
- `internal/agent/runner/task_runner.go` (:322 `makeTaskRunner`, :570-673 gauntlet)
- `internal/agent/supervision/registry.go` (generation-less map)
- `internal/tools/supervisor.go` (5 actions; `halt` only blocking path)
- `internal/agent/pipeline/trace.go`, `internal/agent/supervision/snapshot.go` (effect stream)
- `goatherd-runtime v0.27.0` — `scope.go` `forkWithBaseline` atomic fork record
