# Context/effect fold — adopt/decline decision

Status: **declined** — `yaah-hxk3.4` (spike `yaah-1164`)
Reference: Shepherd v0.3.1 at `.scratch/shepherd/`

## Decision

Do not port Shepherd's context/effect fold into yaah. No context is ported.
This page is the adopt/decline ledger for all six contexts; the verdict is
against the fold, not against the individual surfaces yaah already has.

## What the fold is

Shepherd routes every boundary crossing — provider calls, world operations,
retained captures, settlement — through one typed effect channel where it is
watched, refused, and recorded. Six `ExecutionContext` implementations sit
behind that channel, each exposing domain effects.

## Adopt/decline per context

| Context | Shepherd effects | yaah equivalent | Verdict |
|---|---|---|---|
| Workspace | `WorkspacePatchCaptured`, `BashCommand`, `DiffPatch` | substrate sandbox + kernel world | **Declined** — already covered |
| Session | `SessionCreated` / `SessionForked` / `SessionResumed` | `internal/memory` + `internal/sessions` (SQLite) | **Declined** — already covered |
| MCP | `MCPServerConnected`, `MCPToolCalled` | `internal/mcp` (client + server, stdio/HTTP) | **Declined** — already covered |
| Database | `QueryExecuted` (read-only SQL) | — | **Declined** — no generic SQL context |
| KV store | `KeySet`, `KeyDeleted` | — | **Declined** — no KV abstraction |
| App store | `AppStoreAPICall` | — | **Declined** — out of scope |

## Rationale

- **The observation property is already present.** yaah records every boundary
  crossing through the goatherd-kernel effect bus and the run trace, so the
  "boundary is the record" guarantee Shepherd's effect model exists to provide
  is not a capability yaah is missing.
- **The six contexts are plumbing, not semantics.** Workspace, session, and MCP
  are served by yaah's own surfaces; database/KV/app-store are absent by design
  or out of scope. Porting the fold would add a second authority/observation
  surface beside the kernel bus with no new capability behind it.
- **No integration seam pays for itself.** The spike's mandate was to adopt only
  semantics that improve deterministic safety or review quality; none of the
  six contexts did.

## Consequences

- No yaah code or configuration changes.
- The scorecard row for "Contexts / effect fold" is marked **Declined**.
- A future first-class context (e.g. a read-only SQL context) is a standalone
  feature bead, not a re-opening of this fold.
