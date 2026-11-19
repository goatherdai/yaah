# Workflow DSL Architecture Audit — `internal/workflow`

**Date:** 2026-11-18
**Commit audited:** `b63735c9`
**Method:** direct inspection of every package's public surface, the core
reducer, IR, host door, persistence, and the validator families; dependency
graph verified with `go list`; `go build`, `go vet`, `gofmt`, and
`go test -cover` run across the tree. Read-only; nothing was modified.

**Scope:** `internal/workflow/{spec,ir,validate,exec,host,registry,store,replay,assert,trace}`
(~22.9k LOC production, ~14k LOC test) and the CLI composition root
`cmd/yaah/workflow*.go` (~4.1k LOC). Complements, and does not replace, the
repo-wide `docs/architecture-audit-2026-11.md`, which predates the DSL.

---

## 1. Executive summary

The workflow DSL is architecturally strong and unusually disciplined for its
size. The dependency graph is a genuine one-way stack with no cycles, the
deterministic-reducer design is real rather than aspirational, and the
port/adapter seam at `exec.Runtime` is textbook hexagonal. The documentation
is the best in the repo: package `doc.go` files encode invariants and the
*why* behind them, and the claims are pinned by tests (for example
`host/envelope_parity_test.go` decoding a real dispatch envelope into the
scheduler's mirror).

The debt is concentrated in three places: **type-level duplication between
`spec` and `ir`**, one **150-method `Run` god type**, and a few
**cross-package string contracts** where structured data would be safer. None
is a correctness bug today; each gets more expensive as the node vocabulary
grows.

| # | Sev | Finding | Headline |
|---|-----|---------|----------|
| W1 | P2 | Parallel `spec`/`ir` enum trees with ~15 hand-written converters | New enum value fails at runtime, not compile time |
| W2 | P2 | `ir.Node` is a flat sum-type-in-one-struct | Type system does not enforce the closed vocabulary |
| W3 | P2 | `Run` is a 150-method god type | Broad surface, single-owner reducer |
| W4 | P2 | `store` and `trace` have 0% direct coverage | Durability boundary untested |
| W5 | P2 | Call outcome serialized to a string, re-parsed by `replay` | Fragile cross-package contract |
| W6 | P3 | Validation pipeline duplicated in three places | Drift risk when adding a family |
| W7 | P3 | Four files over 850 LOC | `proposal.go`, `settle.go`, `run.go`, `host.go` |
| W8 | P3 | Artifact path containment is lexical-only | Security-relevant surface, hand-rolled check |

**Build and test status:** `go build ./...`, `go vet`, and `gofmt -l` are
clean; `go test ./internal/workflow/...` passes. Coverage is 73–89% on the
logic packages; `store` and `trace` report 0% because they carry no test
files of their own (see W4).

---

## 2. System map

```
spec  (leaf; no internal deps)
 └─ ir                     resolved, immutable graph + compiled templates/selectors
     ├─ validate           structural / values / linearity / authority / cost
     │   └─ registry       call-name → validated subworkflow document
     ├─ exec               deterministic scheduler core
     │   ├─ store          SQLite checkpoint adapter (over internal/memory)
     │   └─ replay         --replay door over a recorded checkpoint
     └─ host               dispatch-lane door + artifact writer

cmd/yaah/workflow*.go      CLI: flags, file loading, formatting, composition
```

Verified dependency facts:

- `exec` imports **only** `ir`, `trace`, and `assert` from yaah
  (`go list -deps ./internal/workflow/exec | grep yaah`); it does **not**
  reach `internal/tools` or `internal/dispatch`. The package's central
  invariant holds.
- `host` is the only workflow package coupled to the heavy runtime
  (`internal/dispatch`, `internal/jobs`, and transitively
  `internal/tools/substrate`), exactly as its `doc.go` argues. `replay` and
  the test fake implement `exec.Runtime` without any of it.
- `trace` is a true leaf (stdlib only), so `exec` projects records without a
  back-edge into the scheduler.
- No import cycles; the graph is a strict layering.

### 2.1 Size and shape

| Package | Prod LOC | Coverage |
|---|---:|---:|
| `spec` | 1,539 | 80.1% |
| `ir` | 1,962 | 73.1% |
| `validate` | 6,581 | 89.0% |
| `exec` | 10,098 | 77.1% |
| `host` | 1,088 | 79.4% |
| `registry` | 285 | 82.5% |
| `replay` | 327 | 78.4% |
| `assert` | 467 | 87.9% |
| `store` | 310 | 0.0% |
| `trace` | 211 | 0.0% |
| **total** | **22,868** | — |

`store` and `trace` carry no test files of their own (see W4).

Largest production files: `validate/proposal.go` (1146), `exec/settle.go`
(1070), `exec/run.go` (1058), `host/host.go` (863), `validate/authority.go`
(718), `exec/fanout.go` (700).

---

## 3. Strengths

### 3.1 A real deterministic reducer

`Run.Apply` is the sole state mutator (`exec/run.go:300`). Derived events
(`node_ready`, `node_succeeded`, `node_failed`, `node_skipped`,
`retry_scheduled`) are rejected as input (`exec/run.go:311`), so a caller
cannot state a conclusion the reducer did not reach. The driver
(`exec/driver.go`) is the only goroutine that touches a `Run`, so there is no
lock and the event sequence is reproducible. The ready queue is kept in
canonical node-path order (`insertReady`/`dropReady`), so two runs of one
graph with differently ordered `needs` start the same node first.

### 3.2 A narrow start/poll door

`exec.Runtime` (`exec/runtime.go`) is start/poll rather than blocking, which
keeps the scheduler single-threaded. The contract is precise: `Start` must not
block, `Completed` must be stable, and `Block` must not return without a
completion. `host.Runtime` implements the wake as a generation-swap channel
(`host/host.go:105-116`, `store`/`Block`) rather than a reset counter, and
the comment documents the exact failure mode (a stale count making `Block`
return nil and the scheduler abandoning a live attempt) that the swap
prevents. `replay.Runtime` implements the same door with zero model calls as
a property of the type, not of configuration.

### 3.3 Write-ahead persistence

The ordering is the idempotency argument: `dispatch_started` is folded before
`Runtime.Start` (`exec/driver.go:212`), `settlement_asked` is folded before
the door is called, and `Apply` checkpoints after folding and before
returning (`exec/run.go:300`, `exec/persist.go:184`). A crash at any boundary
resumes to exactly the state the reducer had committed. `Resume` refuses a
finished run, a spec-hash mismatch, and an interrupted settle ask, and
re-dispatches an in-flight attempt under a fresh number rather than reusing
the id the door already saw.

### 3.4 Validation as pure functions over the IR

Each family is a plain `func(*ir.Graph) []Diagnostic` — no framework, no
reflection (`validate/doc.go`). Diagnostics are deterministic and grouped by
rule in catalog order. The proposal-linearity analysis reuses the scheduler's
own join semantics ("the way the scheduler joins steps") rather than a second
model of control flow, which is the right call: proving a property against an
order the runtime will not perform proves nothing.

### 3.5 The core stays a leaf

`exec` carries no concrete tool bindings and no DB. The completion crosses
the boundary as the `Completion` mirror (`exec/envelope.go`), and
`host/envelope_parity_test.go` decodes a real `dispatch.DispatchResult`
envelope into it, failing if a scheduler-visible field moves. This is the
mechanism that lets the deterministic core exist without dragging in the
runtime.

### 3.6 Tests with adversarial corpora

Test-to-code ratio is ~0.61 overall, with `validate/testdata/adversarial/*`
(path escapes, cost bombs, duplicate outputs, cycles) and fuzz tests for
`spec`, `ir`, and `validate`. The corpus is asserted to be fully covered
(`TestAdversarialCorpusIsFullyCovered`).

---

## 4. Findings

### W1 (P2) — Parallel `spec`/`ir` enum trees with hand-written converters

`spec.NodeKind` and `ir.Kind` are distinct string types with identical
values. The same duplication repeats for `Lane`, `NetworkMode`,
`FailurePolicy`, `ItemFailure`, `Join`, `SettleAction`, `ConflictPolicy`,
`EmptyPolicy`, `SettleOrder`, `SelectPolicy`, `SettlePolicy`,
`AggregateStrategy`, `OutputType`, `ParamType`, and `GrantAccess`.
`ir/convert.go` is ~250 LOC of one-to-one mechanical mapping.

**Evidence:** `ir/graph.go:16-185`, `ir/convert.go`.

**Why it matters:** adding a value to a `spec` enum without adding it to the
`ir` enum compiles fine and fails at runtime through `resolveErrf`. The
converter's default arms are documented as "defensive: they surface bugs in
resolve," but a forgotten value is an authoring-time surprise, not a resolve
bug.

**Fix direction:** where the authoring and resolved vocabularies are truly
identical, use type aliases (`type Kind = spec.NodeKind`) and keep distinct
types only where resolution actually transforms meaning. Otherwise generate
the converters or collapse to one enum package. At minimum, add an
exhaustiveness test per enum so a new `spec` value without an `ir` mapping
fails `go test`.

### W2 (P2) — `ir.Node` is a flat sum-type-in-one-struct

One struct carries every kind's payload: `Role/Prompt`, `Over/Body/Collect`,
`Cases`, `Until`, `Assert/Candidates/Judge`, `Proposals/Where/Action`,
`Inputs/Strategy`, `Workflow/CallParams`, `Value/ArtifactPath`
(`ir/graph.go:257-340`). The type system does not enforce which fields belong
to which kind; `classifyKind` (`exec/kinds.go:141`) and validator WF014 do.
A wrong-kind read yields a silent zero value unless a validator catches it.

**Evidence:** `ir/graph.go:257-340`; WF014 in `validate/doc.go`.

**Why it matters:** the closed vocabulary is a load-bearing design decision
(WD1), but it is enforced by convention plus a validator that runs in a
different package. Hand-built graphs (tests, future callers) bypass
`ir.resolveNode` and can construct a `Node` with mismatched fields.

**Fix direction:** no rewrite now. Add a construction-time assertion in
`ir.resolveNode` that only kind-relevant fields are set, and add a test that
hand-built graphs with mismatched fields are rejected or refused. Treat WF014
as a hard gate, not a warning. If the vocabulary grows, revisit per-kind
payload structs behind a sealed interface.

### W3 (P2) — `Run` is a 150-method god type

`Run` has 150 methods across 16 files (`grep -c '^func (r \*Run)'`). The
state is genuinely one run, so single ownership is defensible and the
single-mutator invariant argues against splitting the reducer. But the method
surface is the de-facto package API, and many methods serve exactly one kind
(`fanout.go`, `loop.go`, `join.go`, `branch.go`, `select.go`, `gate.go`,
`aggregate.go`, `settle.go`, `call.go`).

**Evidence:** 150 `func (r *Run)` methods; per-file counts from 2 (`emit.go`)
to 25 (`run.go`).

**Why it matters:** onboarding cost and review surface. A reader must hold
the whole method set to reason about any one kind, and sub-ledgers cannot be
tested in isolation.

**Fix direction:** extract cohesive sub-ledgers into small structs `Run`
delegates to — the settle ledger (`open/claims/settled/abortAsked` plus
`settleStep`/`onSettlement*`), the join ledger, and the instance registry.
This reduces `Run`'s surface without splitting the reducer's single-mutator
invariant. Treat as a pressure point, not a defect.

### W4 (P2) — Durability boundary is untested directly

`store` (310 LOC: checkpoint → rows → checkpoint, idempotence, write-ahead
ordering) and `trace` (record vocabulary) have 0% direct coverage. `store` is
exercised only through `host` integration tests; `exec/persist_test.go` uses
a fake store, so the SQLite mapping itself is unverified.

**Evidence:** `go test -cover`: `store 0.0%`, `trace 0.0%`; `ls
internal/workflow/store` shows only `store.go`.

**Why it matters:** this is the one place a silent mapping bug corrupts a
resumed run. The package's own doc claims atomic-per-call and idempotent
re-save semantics; neither is pinned by a test in the package.

**Fix direction:** add a `store` round-trip test: save → load → resume
equivalence, idempotent re-save producing identical rows, and a checkpoint
written mid-teardown resuming correctly. A small `trace` vocabulary test
(each `Kind` derives from exactly one event kind) would close the other gap.

### W5 (P2) — Call outcome serialized to a string, re-parsed by `replay`

A call node's child outcome is composed into `NodeState.Reason` as
`"call_failed: subworkflow <name> (run <id>)[ — <child reason>]"`, and
`replay` reconstructs the child reason by prefix-matching that string
(`replay/replay.go:301-316` against `exec/call.go:108`).

**Why it matters:** the child status and reason are structured facts that
happen to be flattened into prose. `replay` must know exec's exact reason
composition; a wording change silently degrades a replay's child reason to
the verbatim string. This is a cross-package contract expressed as string
formatting.

**Fix direction:** add `ChildStatus`/`ChildReason` fields to `NodeState`
(alongside the existing `CallRunID`) and let `Reason` be a rendering. `replay`
reads the fields, not the prose.

### W6 (P3) — Validation pipeline duplicated in three places

The `Structural + Values + Linearity` chain is written out in
`validate.Validate` (`validate/validate.go:57-59`), `registry.Load`
(`registry/registry.go:211-213`), and `cmd/yaah/workflow_analysis.go:184-186`.

**Why it matters:** adding a document-intrinsic family means remembering three
call sites. The three starts differ (bytes vs name vs doc+params), which is
why `Validate` is not reused, but the shared middle is identical.

**Fix direction:** extract `validate.Intrinsic(g, doc) []Diagnostic` and call
it from all three.

### W7 (P3) — Four files over 850 LOC

`validate/proposal.go` (1146), `exec/settle.go` (1070), `exec/run.go` (1058),
`host/host.go` (863).

**Why it matters:** these are the densest files in the subsystem and the most
likely to hide a change. `proposal.go` (token construction / path-set join /
rule evaluation) and `settle.go` (handle resolution / filtering / value
projection) are the two natural splits.

### W8 (P3) — Artifact path containment is lexical-only

`resolveArtifact` (`host/host.go:849-862`) mixes `filepath.IsAbs`, a `/`
prefix check, and a manual Windows drive check (`declared[1] == ':'`), then
checks `filepath.Rel` containment. Symlinks are not resolved; the doc states
this is an authoring surface, not a sandbox.

**Why it matters:** artifacts are a security-relevant write surface, and the
absolute-path predicate is hand-rolled and platform-branched.

**Fix direction:** use `filepath.IsLocal` (Go 1.20+) plus
`filepath.EvalSymlinks` on the parent directory, or state explicitly that
artifacts are authoring-trusted and drop the pretense of containment.

---

## 5. Recommended order

1. **W5** — structured call outcome. Small, removes a real cross-package
   string contract.
2. **W4** — store round-trip test. Protects the durability claim with a
   focused test.
3. **W6** — extract `validate.Intrinsic`. Trivial, eliminates drift risk.
4. **W1** — enum aliasing or generation. Largest mechanical win; do it
   deliberately.
5. **W2 / W3** — construction-time assertion and sub-ledger extraction. Only
   if the vocabulary or team grows; the current design is defensible as-is.
6. **W7 / W8** — opportunistic cleanup.

---

## 6. What not to change

- **Do not** move `exec` toward importing `dispatch` or `tools`. The
  `Completion` mirror plus the parity test is the mechanism that keeps the
  core deterministic; the layering is the design.
- **Do not** replace the single-mutator reducer with per-kind state machines.
  The one-owner model is what makes resume, replay, and determinism provable.
- **Do not** drop the `spec`/`ir` split. It is the right boundary; only the
  enum representation (W1) is duplicated, not the layering.
