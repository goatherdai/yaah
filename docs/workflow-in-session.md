# Running workflows from inside a session — Scope A plan

Status: **implemented (scope A)** · 2026-11-22

This is the concrete implementation plan for the first slice of the
workflow-in-session surface (`docs/workflow-dsl-plan.md` §10, P3). It covers
**Scope A only**: run a workflow spec *file* from a live session, read-only and
file-backed. Scopes B (in-session progress/cancel/persistence) and C
(definitions native to the session) are listed at the end but out of scope here.

---

## 1. Goal and non-goals

**Goal.** A user in the TUI types `:workflow run <spec>` and yaah validates and
executes that file through the exact code path the CLI uses
(`yaah workflow run`), then renders the resulting report as a durable block in
the conversation stream.

**Non-goals (deferred).**

- Authoring or editing spec files from inside a session (no registry, no
  in-session definition storage) — that is Scope C.
- Live per-node progress, cancel, and persistence into the session store as
  first-class objects — that is Scope B.
- REPL, web, and ACP surfaces. The command and control-message plumbing is
  shared, but only the TUI surface is wired in Scope A.

---

## 2. Verified seams this builds on

All references are against the current tree (`github.com/goatherdai/yaah`).

| Concern | Location | What it gives us |
|---|---|---|
| Run path | `cmd/yaah/workflow_run.go:116` `runWorkflowRun(cmd *cobra.Command, path string, opts workflowRunOptions) error` | validate → `host.Execute` → report. Lives in the same package as the TUI driver, so it is a direct call. |
| Runner seam | `cmd/yaah/workflow_run.go:48` `var workflowRunRunner = newStandaloneWorkflowRunner` | Tests already inject a deterministic runner here — that is how the golden end-to-end runs with no model calls, and it is how Scope A is tested. |
| Nil-context safety | `cmd/yaah/workflow_run.go:164` | `ctx := cmd.Context(); if ctx == nil { ctx = context.Background() }` — a synthetic command without a context still runs. |
| TUI command table | `internal/tui/components/command/command.go` (`Cmd` enum, `commandNames`, `takesArg`, `Split`) | Add one enum value, one name entry, and one arg-taking flag. |
| TUI dispatch | `internal/tui/commands.go` `HandleCommand(cmd command.Cmd, arg string)` | The existing `if t.OnX == nil { t.SetEphemeral(...) }` pattern to copy. |
| Callback surface | `internal/tui/app.go` `On*` fields | Add `OnWorkflowRun func(string)`. |
| Driver wiring | `cmd/yaah/tui.go` `runTUI()` | Wires the callback; already holds `sess` and `controlCh`. |
| Control channel | `internal/control/control.go` `Msg` interface (`marker()`); TUI pump in `internal/tui/run.go` → `handleControlMsg` (`internal/tui/control.go`) | One new `Msg` type plus one `case`. |
| **Precedent** | `control.CompactionSummary` → `case *control.CompactionSummary:` in `internal/tui/control.go` | The exact shape needed: durable, render-only, `isMarkdown: true`, `flushPendingTokens()` first, never persisted as chat. |

---

## 3. Changes, in implementation order

### 3.1 `internal/control/control.go` — new message type

Follow the `CompactionSummary` precedent exactly: a concrete type implementing
`marker()`, render-only.

```go
// WorkflowRunReport carries a completed :workflow run report so surfaces can
// render it in the conversation stream. Render-only, like CompactionSummary:
// the report already lives in the run record, so it must never be persisted
// as a chat message.
type WorkflowRunReport struct {
    Path   string
    OK     bool
    Report string // rendered body (markdown)
}

func (*WorkflowRunReport) marker() {}
```

Optionally add a `Header()` helper returning `workflow <path>: <status>`, so
every surface renders the one-line outcome the same way (the CLI already prints
this line; reuse it).

### 3.2 `internal/tui/control.go` — handle it

Add `case *control.WorkflowRunReport:` mirroring `CompactionSummary`:
`flushPendingTokens()`, append the one-line header, then append
`convItem{text: m.Report, isMarkdown: true}`, `markDirty()`, restore activity
to idle. When `!m.OK`, tag the header with `Theme.Error`.

### 3.3 `internal/tui/components/command/command.go` — register `:workflow`

- Add `CmdWorkflow` to the `Cmd` enum.
- Add `{"workflow", CmdWorkflow}` to `commandNames`.
- Add `CmdWorkflow` to `takesArg` (it consumes the rest of the line, i.e.
  `run <spec>`).
- Add an entry to the command palette (`DefaultEntries`) and tab completion
  (`complete.go`) so the command is discoverable.

### 3.4 `internal/tui/commands.go` — dispatch case

```go
case command.CmdWorkflow:
    if t.OnWorkflowRun == nil {
        t.SetEphemeral("workflow not available in this session")
        return
    }
    verb, rest, _ := strings.Cut(arg, " ")
    if verb != "run" || strings.TrimSpace(rest) == "" {
        t.SetEphemeral("usage: :workflow run <spec.yaml|spec.json>")
        return
    }
    t.OnWorkflowRun(strings.TrimSpace(rest))
```

The `run` verb matches the CLI (`yaah workflow run`) and leaves
`:workflow resume` / `:workflow explain` to slot in later without re-plumbing.

### 3.5 `internal/tui/app.go` — callback field

```go
// OnWorkflowRun receives a spec file path the user typed at :workflow run.
OnWorkflowRun func(specPath string)
```

### 3.6 `cmd/yaah/tui.go` — wire it (the only substantive logic)

```go
app.OnWorkflowRun = func(specPath string) {
    // Read-only guard before we spend anything.
    if st, err := os.Stat(specPath); err != nil || st.IsDir() {
        sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
            Path:   specPath,
            OK:     false,
            Report: fmt.Sprintf("cannot read spec: %v", err),
        }, tuiSendTimeout)
        return
    }
    go func() { // do not block the UI; a run can take minutes
        var buf bytes.Buffer
        c := &cobra.Command{Use: "workflow run"}
        c.SetOut(&buf) // capture the CLI's own report text
        c.SetErr(&buf)
        err := runWorkflowRun(c, specPath, workflowRunOptions{})
        sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
            Path:   specPath,
            OK:     err == nil,
            Report: buf.String(),
        }, tuiSendTimeout)
    }()
}
```

`runWorkflowRun` already writes the report through `cmd.OutOrStdout()` and
returns an error carrying the CLI's `finished <status>` line, so the buffer
plus `err` capture everything with zero refactoring of the run path.

---

## 4. Design decisions

1. **Syntax `:workflow run <spec>`** (verb + path) rather than a bare
   `:workflow <spec>`. It matches `yaah workflow run` and leaves
   `:workflow resume` / `explain` slot-in for later.
2. **Run records still land in the default workflow store** (`state.db`).
   "Read-only" scopes the *spec* (we run a file, never author in-session);
   writing the run record is what enables `workflow resume` and
   `explain --from-trace` later, so we keep it. A throwaway in-memory store is
   the fallback if a strictly side-effect-free A is preferred.
3. **No live progress in A.** The report appears once, on completion. Per-node
   streaming is the Scope B hook (a `control.Msg` progress type over the
   existing channel — the seam is present, it is simply not built now).
4. **Cancellation deferred to B.** `runWorkflowRun` runs with
   `context.Background()` in A. `OnAbort` → cancel is a small follow-on
   (`cmd.SetContext` with a cancellable context); A lets the run finish.

---

## 5. Correctness gotchas

- **Send the report on `controlCh` directly — never through the agent
  broker/events.** Routing it through the broker trips the AST-based
  `events/exhaustive_test.go` pin and silently vanishes for view-less clients.
- The new `control.Msg` must implement `marker()` and needs only a TUI `case`;
  web and ACP have their own `handleControlMsg`-style switches and must
  default-ignore unknown types — verify none of them panic on an unmatched
  case when the type is added.
- The report buffer is plain text the TUI markdown renderer already handles;
  render with `isMarkdown: true` exactly like `CompactionSummary`, without
  tview tag wrapping (otherwise color codes render literally).

---

## 6. Tests

- `internal/tui/components/command/command_test.go`:
  `Split("workflow run a.yaml")` → `CmdWorkflow` with the `run a.yaml` arg;
  `Name(CmdWorkflow) == "workflow"`.
- `internal/tui/commands_test.go`: nil `OnWorkflowRun` → ephemeral
  "not available"; missing/bad arg → usage line.
- `internal/tui/control.go` test: `WorkflowRunReport` appends a durable
  markdown entry and does not reach the chat-persist path.
- `cmd/yaah`: a driver-level test reusing the existing golden end-to-end —
  inject a deterministic runner via the `workflowRunRunner` seam (its stated
  purpose) and assert the callback sends a report with `OK == true` for
  `internal/workflow/spec/testdata/valid/*`.

---

## 7. Effort

Six files, mostly mechanical; the only real logic is the ~20-line callback in
`cmd/yaah/tui.go`. Fits in one focused `golang-developer` dispatch.

---

## 8. Follow-on (out of scope)

- **Scope B** — proper in-session feature: per-node progress via a
  `control.Msg` progress type, `OnAbort`-driven cancellation, run persistence
  surfaced through the session, and REPL/web/ACP carriage of the same messages.
- **Scope C** — definitions native to the session: author/edit/store specs
  in-session, a spec registry, and a repair loop.
