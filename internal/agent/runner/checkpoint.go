package runner

import (
	"context"

	goatherdsandbox "github.com/goatherdai/goatherd-runtime/sandbox"
	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/agent/pipeline"
)

// GoatherdTurnCheckpointer adapts agent.TurnCheckpointer to the shared
// goatherd-runtime ScopeManager. It records workspace checkpoints (plus
// opaque snapshots) on a single scope, using whatever Sandbox that scope holds
// — an in-place repository, or the sub-agent's isolated worktree.
type GoatherdTurnCheckpointer struct {
	mgr     pipeline.ScopeSource
	scopeID string
	sandbox goatherdsandbox.Sandbox
}

// NewGoatherdTurnCheckpointer returns a turn checkpointer that creates
// single-use checkpoints on scopeID against sb.
func NewGoatherdTurnCheckpointer(mgr pipeline.ScopeSource, scopeID string, sb goatherdsandbox.Sandbox) *GoatherdTurnCheckpointer {
	return &GoatherdTurnCheckpointer{mgr: mgr, scopeID: scopeID, sandbox: sb}
}

// Checkpoint creates a single-use workspace checkpoint carrying snapshot.
func (c *GoatherdTurnCheckpointer) Checkpoint(ctx context.Context, snapshot []byte) (string, error) {
	cp, err := c.mgr.CreateCheckpoint(ctx, c.scopeID, snapshot)
	if err != nil {
		return "", err
	}
	return cp.ID, nil
}

// Restore rewinds the workspace and returns the stored snapshot. The
// checkpoint is consumed by this call.
func (c *GoatherdTurnCheckpointer) Restore(ctx context.Context, id string) ([]byte, error) {
	return c.mgr.RestoreCheckpoint(ctx, id)
}

// Prune discards every live checkpoint on the adapter's scope.
func (c *GoatherdTurnCheckpointer) Prune(ctx context.Context) error {
	// The prune removes entries in memory first and only then deletes the
	// persisted blobs; on a cancelled run context (timeout exits are the
	// pruned case) the SQL delete would be skipped while the adapter still
	// reported success, leaking blobs. Cancel-free, like the checkpoint
	// creation it cleans up after.
	c.mgr.PruneCheckpoints(context.WithoutCancel(ctx), c.scopeID)
	return nil
}

var _ agent.TurnCheckpointer = (*GoatherdTurnCheckpointer)(nil)
