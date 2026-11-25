package tools

// supervise_scope.go is the supervise tool's membership boundary (yaah-fy28,
// design §4.7): the batch binds to the LIVE DESCENDANTS of the calling scope,
// and the root scope is never in it.
//
// The kernel scope tree is the only place the parent relation exists — the run
// registry knows scope IDs and run metadata, not who forked whom — so the
// filter is a read of the session's ScopeManager joined onto the snapshot the
// Snapshotter already produces. Filtering the SNAPSHOT SOURCE (rather than the
// decisions) is what makes the boundary airtight: a non-descendant never
// appears in the text the model reads, so naming one cannot resolve to a scope
// the batch would act on, and the arbiter reports it as a stale name instead of
// an intervention on a worker this loop does not own.

import (
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// maxScopeDepth bounds the parent walk. A scope tree is finite by
// construction; the bound only stops a corrupted parent pointer from spinning
// a snapshot read, which is a poll on the model's turn path.
const maxScopeDepth = 64

// DescendantSnapshot restricts a snapshot source to one scope's descendants:
// a source that says which scopes the calling loop may supervise.
//
// It keeps FINISHED descendants as well as live ones. A finished worker's
// facts are the last thing a supervisor reads — "what did it actually do" — and
// the batch's own membership rule (live, non-root) is what stops it being a
// target.
type DescendantSnapshot struct {
	// Source is the session's snapshot source (the Snapshotter, normally).
	// Nil yields an empty snapshot.
	Source supervisionapi.SnapshotSource

	// Scopes is the session's scope tree — the parent relation. A
	// DescendantSnapshot cannot prove any scope is a descendant without it, so
	// a nil tree yields an EMPTY snapshot: the conservative failure. Reporting
	// "no workers" makes a batch report Done immediately, which is visible to
	// the caller; reporting "every worker" would hand one loop another loop's
	// workers, which is not.
	Scopes *goatherd.ScopeManager

	// Caller is the calling scope whose strict descendants are kept. Empty
	// yields an empty snapshot: nothing is a descendant of nothing.
	Caller string
}

// Snapshot reads the source and keeps the calling scope's descendants.
func (d *DescendantSnapshot) Snapshot() supervisionapi.Snapshot {
	if d == nil || d.Source == nil {
		return supervisionapi.Snapshot{}
	}
	snap := d.Source.Snapshot()
	descendants := d.descendants()
	if len(descendants) == 0 {
		// Nothing is provably this caller's: report an empty batch rather
		// than the whole session's workers.
		snap.Agents = nil
		return snap
	}
	kept := make([]supervisionapi.AgentSnapshot, 0, len(snap.Agents))
	for _, agent := range snap.Agents {
		if !descendants[agent.ScopeID] {
			continue
		}
		kept = append(kept, agent)
	}
	snap.Agents = kept
	// Dropped stays as the source reported it: it counts agents the BUILDER
	// could not fit, which is a different fact from what this filter removed.
	return snap
}

// descendants is the set of scope IDs strictly below the caller. Recomputed
// per read on purpose: a worker dispatched while the loop is running joins its
// caller's subtree, and the loop should see it on the next tick without
// re-binding (the binding is the batch and its caller, not a frozen list).
func (d *DescendantSnapshot) descendants() map[string]bool {
	if d == nil || d.Scopes == nil || d.Caller == "" {
		return nil
	}
	out := make(map[string]bool, 8)
	for _, scope := range d.Scopes.AllScopes() {
		id := scope.ID()
		if id == d.Caller {
			continue
		}
		for parent, depth := scope.Parent(), 0; parent != nil && depth < maxScopeDepth; parent, depth = parent.Parent(), depth+1 {
			if parent.ID() == d.Caller {
				out[id] = true
				break
			}
		}
	}
	return out
}
