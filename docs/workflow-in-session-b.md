# Running workflows from inside a session — Scope B plan (workflow tool + deterministic launcher)

Status: **draft — proposed, not yet implemented** · 2026-11-22

This revises the Scope A follow-on (`docs/workflow-in-session.md` §8) around a
direction change: instead of a detached, fire-and-forget run, the workflow
becomes a **tool of the session's meta agent (the orchestrator)**, with live
per-node observability and a deterministic CLI launcher. It supersedes the
Scope A `:workflow run` colon command as the long-term surface (that command is
kept; see §9).

---

## 0. Why the pivot

- **Architecture.** The workflow DSL plan (WD6/WD9) makes the orchestrator the
  driver and frames the DSL as *an artifact the orchestrator emits and the host
  validates/executes — "not a cage around the orchestrator."* A workflow that
  "takes over the session" inverts that; a workflow *tool* respects it.
- **Composition.** A tool lets the orchestrator run a workflow, read its
  report, and continue — including chaining. The result lands in the agent's
  context, which is what "in session" should mean.
- **Observability.** The tool-call surface is already first-class
  (`internal/tui/blocks.go`: `AddToolStart`/`AddToolEnd`). A tool gets that for
  free; only the *internal per-node* progress is new work — and that is the
  same work a takeover would need too.

---

## 1. Goal and non-goals

**Goal.** The meta agent can invoke `workflow_run(<spec>)` as a tool; the run
is observable (active tool block + per-node progress) in the TUI; and
`yaah tui workflow run <spec>` opens a session and kicks the workflow off
deterministically.

**Non-goals (deferred).**

- Definitions native to the session (author/edit/store in-session, spec
  registry) — Scope C.
- Changing the headless CLI (`yaah workflow run <spec>` stays exactly as it is).
- REPL/web/ACP carriage of workflow progress — TUI first, the control-message
  plumbing is shared.

---

## 2. The three pieces

1. **A — the `workflow_run` tool** (orchestrator belt).
2. **B — per-node progress** over the control channel.
3. **C — the deterministic launcher** `yaah tui workflow run <spec>`.

A is the pivot; B is the observability that makes it worth doing; C is the
deterministic door into A.

---

## 3. Piece A — the tool

New `internal/tools/workflow_run.go`, mirroring `DelegateTool`
(`internal/tools/delegate.go` — `Name`/`Description`/`Schema`/`Execute(ctx,
args) (string, error)`), because the orchestrator already invokes `delegate`
the same way and its observability is the precedent.

```go
type WorkflowRunTool struct{}

func (t *WorkflowRunTool) Name() string { return "workflow_run" }
func (t *WorkflowRunTool) Description() string { return prompts.ToolDescription("workflow_run") }
func (t *WorkflowRunTool) Schema() json.RawMessage {
    return json.RawMessage(`{
        "type": "object",
        "properties": {
            "spec": {"type": "string", "description": "Path to the workflow spec file (yaml or json)"},
            "strict": {"type": "boolean", "description": "Fail on warnings (optional)"}
        },
        "required": ["spec"]
    }`)
}

func (t *WorkflowRunTool) Execute(ctx context.Context, args string) (string, error) {
    var p struct {
        Spec   string `json:"spec"`
        Strict bool   `json:"strict"`
    }
    if err := StrictToolArgs("workflow_run", t.Schema(), args, &p); err != nil {
        return "", err
    }
    // Reuse the CLI run path: runWorkflowRun(cmd, p.Spec, workflowRunOptions{strict: p.Strict})
    // capture the report into a buffer, return it as the result string.
}
```

Changes:

- Register `"workflow_run"` in the tool registry map
  (`internal/tools/tools.go`, the `"delegate": func() Tool {...}` table).
- Add `workflow_run` to `internal/prompts` (`ToolDescription`).
- Availability: orchestrator/primary agent only, not sub-agents — mirror how
  `delegate` is gated (the meta agent dispatches; a sub-agent must not be able
  to kick off a nested workflow).

The report is returned as the tool result (a normal string — no new event
type), so it rides the existing tool-result path and lands in the agent's
context.

---

## 4. Piece B — per-node progress

The tool result shows the *outcome*; per-node progress shows the *run*. Two
surfaces, two channels:

- **Tool-call surface** (already exists): the TUI renders the active
  `workflow_run` block from `AddToolStart` through `AddToolEnd` with the final
  report.
- **Per-node progress** (new): a render-only `control.Msg` carrying node-level
  state (which node dispatched, which settled, gate/select outcomes), emitted
  by the host as the graph executes and rendered as a live block in the TUI.

The host (`internal/workflow/host`) and/or executor need a **progress sink** —
an optional callback the tool wires to the session's control channel, so node
transitions are published as they happen. This is the one real new seam in the
executor.

---

## 5. Piece C — the launcher

`tuiCmd` (`cmd/yaah/tui.go`) gains a `workflow run <spec>` subcommand (or a
`--workflow <spec>` flag) carrying an initial spec into `runTUI`. After the
session boots, inject the workflow. Two strategies:

- **Prompt-seed (v1, recommend first).** Seed the first prompt
  "Run the workflow at `<spec>` using the workflow_run tool." Reuses the
  existing `sess.RunPrompt` path — zero new session plumbing. The agent, which
  has the tool, invokes it.
- **Deterministic pre-seed (v2, follow-on).** Synthesize the invocation as the
  session's first assistant tool-call, bypassing the agent's round-trip. Exact
  determinism, but needs a small session-init hook to inject a synthetic
  tool-call into the message/event stream; flag whether that hook exists before
  committing to it.

For the CLI form the intent is explicit, so v2 is the eventual target; v1 is
the cheap first cut that proves the tool path end-to-end.

---

## 6. Design decisions

1. **Blocking tool call.** `workflow_run` blocks until the run completes
   (like `delegate` and long `bash` commands). The agent is "inside the tool"
   for the run's duration, which the TUI already renders as an active block;
   per-node progress streams over `control.Msg` during that block. An async
   (background-job) form is possible but adds a job-result handoff for no
   near-term benefit.
2. **Bound the tool result.** The report becomes part of the assistant message,
   which is subject to the assistant-message size ceiling
   (`capAssistantContent`). Truncate or summarize the returned report so a large
   run cannot poison the message; the full report stays in the run record
   (`workflow resume` / `explain --from-trace`).
3. **Progress on `control.Msg`, never the broker.** See §7.
4. **The headless CLI is untouched.** `yaah workflow run` keeps using the
   standalone runner. The tool reuses `runWorkflowRun`, so both share the seam;
   only the *carrier* differs (buffer→tool-result vs. buffer→stdout).

---

## 7. Correctness gotchas

- **Do not add event types for progress.** `internal/agent/events/exhaustive_test.go`
  pins every `Event` ("When you add an Event, ADD IT HERE"). Per-node progress
  must be a `control.Msg`, not an `Event`, and must not route through the agent
  broker — a new event would trip the exhaustive pin and silently vanish for
  view-less clients.
- **`OnToken != nil` is the de-facto streaming switch.** Anything keyed on the
  streaming path disappears for view-less clients and Send-only providers (the
  compaction client). Progress must travel on the control channel, which is
  independent of streaming.
- **The tool result is a plain string**, not a new type — so it needs no
  exhaustive-test change and no consumer-switch change.

---

## 8. Tests

- **Tool** (`internal/tools/workflow_run_test.go`): `Execute` with a golden spec
  under `internal/workflow/spec/testdata/valid/` and a deterministic runner
  injected via the `workflowRunRunner` seam → result carries `OK`; bad path →
  error.
- **Progress**: a host/exec test asserting the progress sink is invoked per
  node transition (or, at the TUI layer, that `control.WorkflowProgress`
  appends a durable/updating block).
- **Launcher** (`cmd/yaah`): `runTUI(initialSpec)` seeds the session (prompt or
  synthetic tool-call) — assert the seeded input carries the spec.

---

## 9. Relationship to Scope A

Scope A's `:workflow run` colon command (PR #350) stays. It is the
fire-and-forget shortcut. The tool is the *in-session* path the orchestrator
uses; once the tool and progress ship, the colon command can be re-pointed at
the same handler so both surfaces share one implementation (the two already
share `runWorkflowRun`).

---

## 10. Effort

- **A** (tool + registry + prompt + tests): ~1 day.
- **B** (host progress sink + `control.Msg` type + TUI render + tests): ~2–3 days.
- **C** (subcommand + seeding): ~1 day.

A and B are independently shippable; C depends on A.

## 11. Out of scope

Scope C — definitions native to the session (author/edit/store in-session, a
spec registry, a repair loop).
