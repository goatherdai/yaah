Manage background sub-agent jobs dispatched via `delegate` with `background: true`. Provides `action: list` (all jobs), `status` (details for one `job_id`), `cancel` (abort a running job), and `wait` (block until a job finishes or `timeout_seconds` elapses — default 60s, max 300s). Use `list` first to discover job ids and pending count.

A job snapshot carries `id`, `role`, `description`, `model` when known,
`status`, `still_running` when true, `duration_ms` when finished, `error`
when it failed, `world` once resolved, and — for a terminal job — `result`
(the bounded head of the runner output) plus `result_available` (`true` when
a non-empty result exists). `list` returns `jobs` and `pending`; `status`
returns one snapshot; `cancel` adds `cancel` and optional `notes`; `wait` adds
`timed_out`, `interrupted`, and optional `notes`.

Job state is reported truthfully. A job that has not finished carries
`still_running: true` alongside its `status`; a `wait` that hits its deadline
reports **both** `timed_out: true` and `still_running: true` (the job is not
done, the *wait call* timed out). A `wait` cut short because the turn was
cancelled or a parent deadline fired reports `interrupted: true` instead of
`timed_out`, again with `still_running: true` — the job was not asked to stop.

`cancel` reports what it actually did in a `cancel` field, in one of exactly
three values:

- `cancelled` — the id named a running job and the request reached it. The job
  may still be winding down; read `status`/`still_running` in the same response
  to know whether it has stopped. A request is not a stop.
- `already_finished` — the job had already reached a terminal state, so nothing
  was interrupted. `status` carries that real final state (`completed`,
  `failed`, `cancelled`).
- `unknown` — no such job: never dispatched in this session, or its finished
  record was reaped. No job fields are reported, because there is no job.

An envelope may carry `notes` when a call returned early or has a caveat: a cancellation request that has not stopped the job yet, a wait interrupted before the job finished, or a job record that disappeared. `notes` explains the early return; it does not change the reported `status` or `still_running` facts. It is absent when there is no caveat.

Unknown argument names are rejected with the list of accepted keys; there is no silent default for a misspelled `job_id` or `timeout_seconds`.

A terminal job's `result` is the head of its output when it fits the status window; a longer one adds a marker naming the total byte count and the route to the rest, and the full text stays on the job record. The snapshot also carries `result_total_bytes` (the full length in bytes), `result_offset` (the byte offset the returned page starts at), `result_limit` (the page window applied), and `result_next_offset` (where the next page begins, absent when the page reaches the end). Pass `result_offset` and `result_limit` with `status` to read an exact page of the full result: such a page carries no marker, so consecutive pages concatenate to the original output exactly.
