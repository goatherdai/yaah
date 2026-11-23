# Citation checking — adopt/decline decision

Status: **declined** — `yaah-hxk3.2` (`yaah-1165`)
Reference: Shepherd `shepherd/extras/citation-checker` (the `check-citations` extra)

## Decision

Decline porting Shepherd's citation checker into yaah. It is a domain-specific
example application, not a framework capability, and its pipeline is coupled to
Python, the Claude Code CLI, and a native jail.

## What Shepherd's checker is

`shepherd-check-citations` verifies the bibliography of an academic paper PDF:
extract references (`pdfplumber`), collect evidence and exact matches, send
unresolved references to an LLM in batches, then report verified /
confirmed-metadata-error / uncertain citations. It ships as an **extra** beside
the runtime, not as a core facility.

## Why decline

- **Domain-specific.** It checks academic paper bibliographies. yaah is a
  general agent harness; a PDF citation verifier is not a capability the
  harness surface needs.
- **Coupling.** The pipeline is Python (`pdfplumber`), the Claude Code headless
  CLI, and a native jail (Seatbelt/Landlock). Porting means re-implementing PDF
  extraction plus a reviewer loop for one example app.
- **The generic idea is already expressible.** Verifying a claim against sources
  is a composition yaah builds from existing tools — `webfetch`/`http` through
  the guarded network path plus the workflow `gate`/`assert` predicates — not a
  hard-wired checker.

## Consequences

- No yaah code or config changes.
- The scorecard row for "Citation checking" is marked **Declined**.
- A future "verify this agent's cited sources" tool, if ever wanted, is a
  standalone feature bead built on the existing guarded HTTP path + workflow
  predicates — not a port of the PDF checker.
