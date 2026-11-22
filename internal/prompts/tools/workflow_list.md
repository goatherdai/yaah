List the workflow documents discoverable in the search directories and return each as `name<TAB>absolute-path`.

The search order is fixed: the workspace's `workflows/` directory, then the global `~/.yaah/workflows/` directory. Documents are deduplicated by base name — an earlier directory wins — and reported sorted by name. The returned names are the handles `workflow_run` accepts, so the intended flow is list first, then run a name from the list.

The result is a directory listing, not a run: nothing is validated, dispatched, or opened. When no documents are found the tool reports that explicitly rather than returning an empty list silently.
