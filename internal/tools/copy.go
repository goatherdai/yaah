package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/goatherdai/yaah/internal/prompts"
)

// CopyTool duplicates a file inside the workspace, leaving the source in place.
//
// Like move, it exists so a sub-agent without a shell can still do ordinary
// file work: bash is blocked for sub-dispatches by policy and the git tool's
// whitelist excludes cp. Argument decoding and every safety rule (path
// validation, files only, no-clobber unless force) come from prepareTransfer in
// move.go, so the two tools cannot drift apart.
type CopyTool struct {
	PV *PathValidator
	WS Workspace
}

var (
	_ PathValidatorSetter = (*CopyTool)(nil)
	_ WorkspaceSetter     = (*CopyTool)(nil)
)

func (t *CopyTool) SetPathValidator(pv *PathValidator) { t.PV = pv }
func (t *CopyTool) SetWorkspace(ws Workspace)          { t.WS = ws }

func (t *CopyTool) Name() string        { return "copy" }
func (t *CopyTool) Description() string { return prompts.ToolDescription("copy") }

func (t *CopyTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Absolute path of the file to copy."
			},
			"to": {
				"type": "string",
				"description": "Absolute destination path for the copy. Its parent directory must already exist; create it with mkdir first."
			},
			"force": {
				"type": "boolean",
				"description": "Replace the destination when it already exists. Default false: an existing destination is refused.",
				"default": false
			},
			"mkdir_p": {
				"type": "boolean",
				"description": "Create missing parent directories of the destination. Default false: the parent must already exist.",
				"default": false
			}
		},
		"required": ["path", "to"]
	}`)
}

// IsDangerous is true for every call: a copy writes a file at the destination.
func (t *CopyTool) IsDangerous(string) bool { return true }

// selfGuardsPath marks copy as taking the per-path lock for both ends inside
// its own Execute (lockConflictPathSet, fileguard.go), so the RecordingTool
// wrapper does not hold the same non-reentrant lock across it. The marker is
// what keeps the lock on EVERY lane: the orchestrator calls this tool directly,
// with no recording wrapper to take it.
func (*CopyTool) selfGuardsPath() {}

func (t *CopyTool) Execute(ctx context.Context, args string) (string, error) {
	req, err := prepareTransfer(ctx, "copy", t.Schema(), workspaceOf(t.WS, t.PV), args)
	if err != nil {
		return "", err
	}
	// Both ends in one sorted acquisition (see lockConflictPathSet) so a copy and
	// a move of the same pair cannot deadlock. The source is included because a
	// concurrent move of it must not race this read, and the destination because
	// the no-clobber check below is check-then-act.
	unlock := lockConflictPathSet(req.Path, req.To)
	defer unlock()
	if err := req.validate(ctx, "copy"); err != nil {
		return "", err
	}
	err = req.ws.Copy(ctx, req.Path, req.To)
	// Publish the destination from inside the lock (write_evidence.go): the
	// recording wrapper cannot take this lock across a self-guarding transfer,
	// so its own after-read could otherwise observe — and claim — a concurrent
	// dispatch's write to the destination (yaah-1615). The source is not
	// published: a copy leaves it untouched, and the recorder must not record it.
	publishPathSnapshot(ctx, req.ws, req.To)
	if err != nil {
		return "", fmt.Errorf("copy: %w", err)
	}
	return fmt.Sprintf("Copied %s to %s", req.Path, req.To), nil
}
