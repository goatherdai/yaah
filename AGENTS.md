# AGENTS.md — yaah

> Notes for AI coding assistants (and humans) working on the yaah codebase.

## What yaah is

yaah is a vendor-free AI agent harness: one Go static binary, minimal config
at `~/.yaah/`, skills at `./.agents/` (project, tracked) and `~/.agents/`
(cross-tool standard, per-machine), and MCP over stdio and HTTP. See
[README.md](./README.md) for the user-facing pitch,
[docs/architecture.md](./docs/architecture.md) for the runtime design, and
[docs/acp.md](./docs/acp.md) for the ACP v1 server surface and client setup.

## Local-only directories

These are local to a developer's working copy. Never commit generated state
from them:

- **`.scratch/`** — scratch space for cloning third-party repos while building
  a skill. Real dependencies are pinned in `go.sum`.
- **`.yaah/`** — local runtime state, including the `goatherd-worktrees/`
  checkouts that proposal settlement creates. Each is a full copy of the repo
  at an older commit, so a repo-wide `find` or `grep` matches every file twice
  and reports stale code as current. Search with `git grep`, which respects
  the ignore rules.
- **`.beads/`, `.dolt/`** — local beads issue-tracker state; the database is
  local even though a few bootstrap files are tracked.
- **`.ghost/`, `.claude/`, `.qwen/`, `.hermes/`, `.kilo/`, `.serena/`,
  `.entire/`, etc.** — metadata for AI coding tools.
- **`pours/`** — local SigNoz telemetry stack deployment config.

## Repo layout

```
yaah/
├── main.go                      # jail.MaybeRunTrampoline(os.Args), then cmd/yaah.Execute()
├── cmd/yaah/                    # cobra commands + composition root
│   ├── root*.go                 # build-time vars, persistent flags, dispatch
│   ├── session*.go              # agentSession plumbing and options
│   ├── wiring*.go               # providers, tools, prompts, OTel, MCP
│   ├── build_loop.go            # per-run LoopBuilder + runPrompt
│   ├── provider_resolve.go      # provider/model/fallback resolution
│   ├── repl_loop.go             # interactive REPL
│   ├── serve*.go                # MCP tool server (stdio + HTTP)
│   ├── acp_cmd.go               # ACP server shim (server in internal/acp)
│   ├── web*.go                  # browser UI + SSE view
│   ├── tui.go                   # terminal UI entry point
│   └── ...                      # config, doctor, skill, mcp, memory, trace, update
├── internal/
│   ├── acp/                     # ACP JSON-RPC server
│   ├── agent/                   # agent loop, events, pipeline, tools, context
│   │   ├── budget/              #   pure sub-agent budget resolution
│   │   ├── context/             #   pure context helpers (tokens, split, prune)
│   │   ├── errorclassify/       #   provider error classification
│   │   ├── events/              #   typed events + hooks
│   │   ├── llm/                 #   LLM client: streaming, retry, fallback
│   │   ├── pipeline/            #   middleware pipeline
│   │   ├── recovery/            #   crash recovery for orphaned scopes
│   │   ├── runner/              #   sub-agent dispatch wiring
│   │   ├── subagent/            #   role definitions and registry
│   │   └── supervision/        # advisory rules and destructive guard
│   ├── atomicfile/              # atomic file replace (Windows replace-retry)
│   ├── banner/                  # startup/status banner rendering
│   ├── conflictapi/             # neutral conflict evidence shared by tools and the engine
│   ├── config/                  # config load, env substitution, validation
│   ├── control/                 # typed control-plane messages (todos, questions, approvals)
│   ├── cwdpath/                 # cwd canonicalization shared by store + filters
│   ├── dispatch/                # dispatch lanes, proposal settlement, git-delta apply
│   ├── doctor/                  # `yaah doctor` checks
│   ├── effectpolicy/            # structural effect-surface authority (Match/Plan) for the sub-agent guard
│   ├── envelopecheck/           # envelope/doc parity gate shared by tools + dispatch
│   ├── instructions/            # AGENTS.md/CLAUDE.md discovery
│   ├── jail/                    # per-platform containment backend selection
│   ├── janitor/                 # dispatch-residue classifier + reclaimer: trace lifecycle + registry + filesystem
│   ├── janitor/                 # dispatch-residue classifier: trace lifecycle + registry + filesystem
│   ├── jobs/                    # background jobs + sub-agent I/O contract
│   ├── mcp/                     # MCP client + server (stdio + HTTP)
│   ├── memory/                  # SQLite sessions, memory, FTS5, embeddings
│   ├── observability/           # OpenTelemetry tracing + span buffer
│   ├── parity/                  # Goatherd parity ledger drift guard
│   ├── permissionplan/          # monitor-assignment compiler unifying jail + settlement authority
│   ├── permissions/             # ordered allow/deny/ask tool rules (leaf package)
│   ├── plans/                   # PLAN.md discovery/parsing
│   ├── process/                 # background process manager
│   ├── prompts/                 # identity, roles, tool docs, prompt assembly
│   ├── providers/               # OpenAI-compatible + Anthropic clients
│   ├── pubsub/                  # typed event broker
│   ├── repl/                    # REPL history, slash commands, colors
│   ├── retention/               # owner-scoped session eviction + GC sweep ordering
│   ├── rolefile/                # role frontmatter parsing/validation
│   ├── runs/                    # protocol-neutral run record (MCP serve, A2A)
│   ├── sandboxfake/             # in-memory sandbox test double
│   ├── sessions/                # engine-level session registry (all views)
│   ├── skills/                  # SKILL.md discovery/parsing
│   ├── spinner/                 # terminal spinner primitives
│   ├── supervisionapi/          # neutral supervision errors and Firing shared by agent and tools
│   ├── todo/                    # todo-list model shared by the views
│   ├── toolargs/                # strict tool-argument decoding, unknown-key errors
│   ├── tracestore/              # trace store open seam: backend + DSN from config → kernel StoreConfig
│   ├── toolcontract/            # neutral Tool/DangerClassifier contracts shared by agent and tools
│   ├── toolfmt/                 # shared tool result formatting
│   ├── tools/                   # built-in tools (dispatch lane is internal/dispatch)
│   │   └── substrate/           #   session sandbox, workspace bootstrap, apply-out
│   ├── tui/                     # tview UI components
│   ├── types/                   # message and chat request types
│   ├── update/                  # release checking and self-update
│   ├── wake/                    # wait_for/watch predicate engine + armed watches
│   ├── workflow/                # typed workflow documents (dir only, no package)
│   │   ├── ir/                  #   resolved immutable workflow graph
│   │   └── spec/                #   typed v1 spec, canonical JSON + spec hash
│   └── worldinfo/               # resolved dispatch-world reporting
├── docs/                        # user and developer docs; see docs/architecture.md
│                                # and docs/configuration.md for the entry points
├── bench/                       # benchmark harnesses
├── tools/cataloggen/            # manual regenerator for providers/catalog/data.json
├── scripts/                     # acp-tck.sh, package-release.sh
├── .agents/                     # project skills and roles; role files carry mode:
├── .github/workflows/ci.yml     # CI: gofmt, vet, staticcheck, test, native build (ubuntu/macos/windows)
├── README.md
├── CONTRIBUTING.md
├── SECURITY.md
├── AGENTS.md
└── LICENSE                      # MIT OR Apache-2.0
```

## Build and test

```bash
go build .                      # produces ./yaah
go test ./...                   # all tests
go vet ./...                    # vet
gofmt -l .                      # must be empty
go run honnef.co/go/tools/cmd/staticcheck@latest ./...   # staticcheck

# Cross-compile
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o dist/yaah-darwin-arm64  .
GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/yaah-darwin-amd64  .
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/yaah-linux-amd64    .
GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o dist/yaah-linux-arm64    .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o dist/yaah-windows-amd64  .
```

Install locally:

```bash
go build -trimpath -ldflags '-s -w' -o yaah .
ditto --norsrc yaah ~/.local/bin/yaah  # macOS: avoids Gatekeeper quarantine
```

## Dev loop (MCP hot-reload)

Use the HTTP MCP transport so the host agent can connect once and survive
rebuilds:

```bash
yaah mcp serve --listen 127.0.0.1:7333
```

After a code change, rebuild and restart only the server process; the host
agent reconnects on the next request. Verify the swap via `status`
(`pid` must change), then inspect behavior with `traces` (`tree: true` on a
known `trace_id`). One observable change per iteration; trust trace data over
model self-report. Full workflow: `.agents/skills/yaah-dev-loop/SKILL.md`.

`cmd/yaah/serve_tools.go` registers the same tools for stdio and HTTP via
`registerServeTools()`.

## Conventions

- **Go 1.26.3+** (`go.mod` requires 1.26.3).
- **No `go generate` directives and no build tags.** The one generated file,
  `internal/providers/catalog/data.json`, is produced by hand —
  `go run ./tools/cataloggen`, then commit the reviewed diff. The transform
  is deterministic, and `go run ./tools/cataloggen -verify` reports upstream
  drift without writing anything.
- **cobra + pflag** for CLI.
- **`internal/` for everything private.** `pkg/` is reserved for future
  exports.
- **Avoid globals** except build-time vars, persistent-flag parse targets,
  the embedded web FS, the atomic role registry, and OTel instruments
  initialised once at startup.
- **Errors are values, not panics.**
- **No third-party HTTP client.** Use `net/http` from the stdlib.
- **`gopkg.in/yaml.v3`** for config parsing.
- **`modernc.org/sqlite`** for SQLite (pure Go, no CGo, FTS5 included).
- **`yzma` + `purego`** for in-process GGUF embeddings
  (`internal/memory/gguf.go`): prebuilt llama.cpp bindings over FFI, so
  semantic memory needs no Ollama or LM Studio and `CGO_ENABLED=0` holds.
- **Every other direct require in `go.mod` is load-bearing** — tview, tcell,
  tvxwidgets, tviewmd, uniseg (TUI); seven `go.opentelemetry.io/otel` modules
  (tracing and metrics); `goatherd-kernel` / `goatherd-runtime` plus four
  sandbox backends (`landlock`, `seatbelt`, `winacl`, `croft`);
  `containerd/v2` + `errdefs` (container lane); `figlet-go` (banner);
  `golang.org/x/tools` (`go_refactor`); `testify` (one test file). A new
  direct dependency needs a reason the stdlib cannot cover.
- **No model SDK for the OpenAI-compatible path.** Direct HTTP to
  `chat/completions` preserves provider extension fields
  (`reasoning_content`, Copilot mode, thinking overrides). The native
  Anthropic path uses `github.com/anthropics/anthropic-sdk-go` for wire types,
  SSE decoding, and typed errors; retries, fallback, and classification stay
  in yaah.

## Style

- Run `gofmt -w .` before committing.
- `go vet` and `staticcheck` must be clean.
- Prefer `for ... range` and early returns.
- One file, one concern.
- Tests live next to the code (`foo.go` ↔ `foo_test.go`).
- Use subtests (`t.Run("name", ...)`) for table-driven cases.

## Engine-view architecture

The agent loop communicates with consumers (TUI, REPL, web, ACP) through a
single typed event interface. View delivery is broker-based; a few control
hooks (`ApproveFn`, `ContinueAfterMaxIter`, `OnUsage`, `OnSubUsage`) remain on
the loop.

### Adding a consumer

Implement `agent.View`:

```go
type View interface {
    HandleEvent(Event)
}
```

The loop creates an internal `pubsub.Broker[Event]` and a `BrokerView`
adapter. Events are typed structs implementing the sealed `Event` interface;
see `internal/agent/events/events.go`.

### Adding an event type

1. Add the struct in `internal/agent/events/events.go` with an
   `eventMarker()` method.
2. Add it to `allEvents()` in `internal/agent/events/exhaustive_test.go`.
   The exhaustive tests fail until every consumer handles the event (an
   explicit "intentionally ignored" case counts).
3. Publish it from the agent loop's internal broker.

### Consumers

| Consumer | View impl | File |
|---|---|---|
| TUI | `App.HandleEvent` (`App` is the `agent.View`) | `internal/tui/proxy.go` |
| REPL | `terminalView` | `cmd/yaah/view_terminal.go` |
| Web | `sseView` | `cmd/yaah/web_view.go` |
| Sub-agents | `agent.NoopView` | `internal/agent/runner/runner.go` |
| MCP serve | `agent.NoopView` | `cmd/yaah/serve.go` |
| ACP serve | `acp.View` + `acp.ViewWithWrite` | `internal/acp/view.go` |

Control-plane messages (todos, questions, approvals, model lists) use
`control.Msg`, a separate channel from broker events.

## Skills

Project skills live in `.agents/skills/`; user skills live in
`~/.agents/skills/`. Load a skill when the task matches its description using
the `skill` tool (`action: "load"`). See the directory for the current list;
`yaah-dev-loop`, `beads`, and `dolt-server-lifecycle` are the core development
skills.

## Beads issue tracker

This project uses **bd (beads)** for task tracking. Run `bd prime` for the
full workflow.

```bash
bd ready                 # available work
bd show <id>             # details
bd update <id> --claim   # claim work
bd close <id>            # complete work
```

Rules:

- Use `bd` for all task tracking. Do not create markdown TODO lists or use
  TodoWrite/TaskCreate.
- Use `bd remember` for durable knowledge; do not create MEMORY.md files.
- Conservative git policy by default: do not commit, push, or run Dolt remote
  sync unless the user asks.

## What not to do

- Don't add a `web/` or `gateway/` package, or integrations for
  Telegram/Discord/Slack.
- Don't add a hosted service or a paid-only integration.
- Don't add Anthropic-specific features outside the provider/MCP surface.
- Don't change the license (`MIT OR Apache-2.0`).
- Don't bump the version in a PR; maintainers cut releases.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:970c3bf2 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   bd dolt push
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->

<!-- BEGIN BEADS CODEX SETUP: generated by bd setup codex -->
## Beads Issue Tracker

Use Beads (`bd`) for durable task tracking in repositories that include it. Use the `beads` skill at `.agents/skills/beads/SKILL.md` (project install) or `~/.agents/skills/beads/SKILL.md` (global install) for Beads workflow guidance, then use the `bd` CLI for issue operations.

### Quick Reference

```bash
bd ready                # Find available work
bd show <id>            # View issue details
bd update <id> --claim  # Claim work
bd close <id>           # Complete work
bd prime                # Refresh Beads context
```

### Rules

- Use `bd` for all task tracking; do not create markdown TODO lists.
- Run `bd prime` when Beads context is missing or stale. Codex 0.129.0+ can load Beads context automatically through native hooks; use `/hooks` to inspect or toggle them.
- Keep persistent project memory in Beads via `bd remember`; do not create ad hoc memory files.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.
<!-- END BEADS CODEX SETUP -->
