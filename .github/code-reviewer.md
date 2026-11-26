# yaah review rules

Extra calibration for reviewing PRs in yaah, a vendor-free agent harness written
in Go. These rules adjust Robin's built-in behavior; they do not replace it.

## Searching the repository

Search with `git grep`, never `grep -r`, `rg`, or a recursive `find`. The working
copy contains `.yaah/goatherd-worktrees/` — full stale checkouts that proposal
settlement creates — plus other generated state under `.yaah/` and `.beads/`.
A repo-wide search matches every file twice and reports stale code as current.
`git grep` respects ignore rules and is safe.

## Documentation drift is a first-class finding

yaah's docs are model-facing: dispatch agents read them as instructions, so a
document that over-states or misstates behavior is a defect, not a nit.

- When a PR changes behavior, check whether the affected claim also appears in
  the other model-facing docs — currently `docs/agent-instructions.md`,
  `docs/sub-agents.md` (including its "Retention and isolation" section), and
  `docs/configuration.md` — and in nearby code comments. Report every location
  of the stale claim in one finding, not one finding per copy discovered.
- Do not flag documentation wording that is merely informal when it is
  directionally accurate.

## Severity calibration for this repository

- **High:** jail/confinement or egress escape; data loss or corruption; a broken
  settlement or seal invariant (a proposal landing twice, a lost attempt, a
  wrong dispatch basis); any path that lets sub-agent output reach the host tree
  unvetted.
- **Medium:** a real defect in changed code — wrong behavior, a route the diff
  claims works but the code refuses, a code path the diff introduces with no
  test.
- **Low:** polish, naming, comment accuracy that cannot mislead an agent, and
  test-coverage gaps for unchanged neighboring code.

Pre-existing problems that merely sit adjacent to the diff are worth at most one
Low note per PR, not one finding per occurrence.

## Tests

yaah's tests are load-bearing regression pins, often table-driven and written as
deliberate tripwires. A missing test for a path the diff adds is Medium; a
missing test for code the diff only touches in passing is Low.
