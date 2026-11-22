Run a workflow document and return its report.

The document is validated exactly as `yaah workflow validate` validates it, and a document with error-severity findings is refused with its diagnostics before anything dispatches. `strict` promotes warnings to refusal. Only a passing document reaches the scheduler, so a refusal result is a validation answer, not a run.

The report carries the run id, the final status and reason, every recorded output binding, the artifacts written and any door-side warnings — the same report the command prints. A run that stopped at the door reports the scheduler's own verdict rather than a bare error.

This runs the document; it does not author one. Workflow specs are files in the workspace, so a document that does not exist yet is a write, not a run.
