package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/goatherdai/yaah/internal/prompts"
)

// MoveTool renames a file inside the workspace.
//
// It exists because a sub-agent often has no shell: the bash class is blocked
// for sub-dispatches by policy, and the git tool's whitelist deliberately
// excludes mv. Without this tool a sandboxed worker could create a file but
// never rename it.
//
// The bytes are moved by the substrate's own rename, so nothing is decoded,
// re-encoded, or passed through the model: a binary file survives intact.
type MoveTool struct {
	PV *PathValidator
	WS Workspace
}

var (
	_ PathValidatorSetter = (*MoveTool)(nil)
	_ WorkspaceSetter     = (*MoveTool)(nil)
)

func (t *MoveTool) SetPathValidator(pv *PathValidator) { t.PV = pv }
func (t *MoveTool) SetWorkspace(ws Workspace)          { t.WS = ws }

func (t *MoveTool) Name() string        { return "move" }
func (t *MoveTool) Description() string { return prompts.ToolDescription("move") }

func (t *MoveTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Absolute path of the file to move."
			},
			"to": {
				"type": "string",
				"description": "Absolute destination path for the file. Its parent directory must already exist; create it with mkdir first."
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

// IsDangerous is true for every call: a move relocates a file the user has.
func (t *MoveTool) IsDangerous(string) bool { return true }

// selfGuardsPath marks move as taking the per-path lock for both ends inside
// its own Execute (lockConflictPathSet, fileguard.go), so the RecordingTool
// wrapper does not hold the same non-reentrant lock across it. The marker is
// what keeps the lock on EVERY lane: the orchestrator calls this tool directly,
// with no recording wrapper to take it.
func (*MoveTool) selfGuardsPath() {}

func (t *MoveTool) Execute(ctx context.Context, args string) (string, error) {
	req, err := prepareTransfer(ctx, "move", t.Schema(), workspaceOf(t.WS, t.PV), args)
	if err != nil {
		return "", err
	}
	// Both ends in one sorted acquisition (see lockConflictPathSet): the guard
	// belongs here, at the tool boundary, so it holds on every lane — an
	// orchestrator move previously did a bare read-then-write that a concurrent
	// sub-agent write could silently revert. Sorted order is what keeps two
	// transfers A→B and B→A from deadlocking.
	unlock := lockConflictPathSet(req.Path, req.To)
	defer unlock()
	// validate runs INSIDE the lock: its no-clobber check is check-then-act, and
	// holding the lock across both the check and the primitive is what stops an
	// in-process writer from slipping a create in between.
	if err := req.validate(ctx, "move"); err != nil {
		return "", err
	}
	if err := req.ws.Move(ctx, req.Path, req.To); err != nil {
		return "", fmt.Errorf("move: %w", err)
	}
	return fmt.Sprintf("Moved %s to %s", req.Path, req.To), nil
}

// transferRequest is the shared, validated form of a move/copy call. The two
// tools differ only in which workspace primitive they invoke, so argument
// decoding and every safety rule live here rather than being copied — a rule
// added to one file cannot be missing from the other.
type transferRequest struct {
	ws     Workspace
	Path   string // resolved source
	To     string // resolved destination
	Force  bool
	MkdirP bool
}

// prepareTransfer decodes a move/copy argument object and resolves both paths
// through the workspace's PathValidator, so deny patterns and workspace-root
// containment apply exactly as they do for read/write/edit/delete.
//
// The safety rules that depend on the filesystem — the source must exist and be
// a regular file, the destination must not be a directory, an existing
// destination is refused unless force is set, and the destination parent must
// already exist — live in transferRequest.validate, which the tools call AFTER
// taking the per-path lock. Splitting the two is deliberate: it is what lets the
// no-clobber check and the substrate primitive share one critical section
// without either call site copying the resolution rules.
func prepareTransfer(ctx context.Context, tool string, schema json.RawMessage, ws Workspace, args string) (transferRequest, error) {
	var params struct {
		Path   string `json:"path"`
		To     string `json:"to"`
		Force  bool   `json:"force"`
		MkdirP bool   `json:"mkdir_p"`
	}
	if err := StrictToolArgs(tool, schema, args, &params); err != nil {
		return transferRequest{}, err
	}
	if params.Path == "" {
		return transferRequest{}, fmt.Errorf("%s: path is required", tool)
	}
	if params.To == "" {
		return transferRequest{}, fmt.Errorf("%s: to is required", tool)
	}

	req := transferRequest{ws: ws, Force: params.Force, MkdirP: params.MkdirP}
	src, err := ws.ResolveWritePath(params.Path)
	if err != nil {
		return transferRequest{}, fmt.Errorf("%s: %w", tool, err)
	}
	dst, err := ws.ResolveWritePath(params.To)
	if err != nil {
		return transferRequest{}, fmt.Errorf("%s: %w", tool, err)
	}
	req.Path, req.To = src, dst
	return req, nil
}

// validate applies the move/copy safety rules to a resolved request. It is
// check-then-act, so both tools call it with the per-path lock already held
// (see MoveTool.Execute): the lock is in-process only, which means an in-process
// writer — the orchestrator lane, another dispatch — cannot create the
// destination between the Lstat here and the substrate primitive below. An
// OUTSIDE writer still can: no portable no-replace rename exists on every
// supported OS, so a concurrent editor or a second yaah instance racing the
// transfer is a documented limitation.
//
// Three rules are enforced for both tools, by construction rather than by the
// substrate's replace semantics (host rename, sandbox mv/cp, and the in-memory
// double all disagree on clobbering, so the rule is applied once, above them):
//
//   - the source must exist and be a regular file as the path layer leaves it —
//     a directory, FIFO, socket, or device is refused with a clear error, which
//     keeps the tool's semantics bounded and the host copy from blocking on a
//     special file. A symlink follows the substrate: the host path validator
//     resolves it first (like every other file tool, transferring the target),
//     while a lexical-path substrate such as the container sandbox refuses it;
//   - the destination must not be a directory; a symlink destination is refused
//     on a lexical-path substrate, while the host resolves a valid link first
//     and applies the clobber rule to its target;
//   - an existing destination is refused unless force is set.
func (req transferRequest) validate(ctx context.Context, tool string) error {
	ws := req.ws
	src, dst := req.Path, req.To

	// Lstat, not Stat: the entry at the path is what a rename moves and a copy
	// reads. The source must be a regular file as the path layer leaves it, so
	// directories are out of scope, a FIFO, socket, or device would block or
	// stream forever, and a symlink would diverge between the substrates (mv
	// moves the link, cp dereferences it). All are refused with a clear error.
	//
	// On the host, ResolvePath has already followed a final symlink, so this
	// sees the link's target and a valid link is transferred like any other
	// file tool. On the lexical sandbox path it sees the link itself, and the
	// IsRegular check refuses it — the documented substrate difference.
	info, err := ws.Lstat(ctx, src)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", tool, src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: %s is not a regular file; %s operates on regular files only", tool, src, tool)
	}

	dstInfo, err := ws.Lstat(ctx, dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The common case: a fresh destination.
	case err != nil:
		return fmt.Errorf("%s: %s: %w", tool, dst, err)
	case dstInfo.Mode()&fs.ModeSymlink != 0:
		// A symlink destination is refused on a lexical-path substrate: mv
		// would replace the link while cp writes through it — and for a link to
		// a directory the transfer would drop the file inside that directory.
		// Accepting one there would make the tools' semantics depend on which
		// primitive runs. On the host this branch is unreachable for a valid
		// link (ResolvePath already followed it, so the clobber rule applies to
		// its target) and a dangling link fails in the path layer; it remains
		// here so a substrate that stops resolving cannot silently accept a
		// link.
		return fmt.Errorf("%s: destination %s is a symlink; pass the real path or remove the link", tool, dst)
	case dstInfo.IsDir():
		return fmt.Errorf("%s: destination %s is a directory", tool, dst)
	case !req.Force:
		return fmt.Errorf("%s: destination %s already exists; pass force=true to replace it", tool, dst)
	}

	// The destination parent must already exist. Creating it here would make
	// copy/move implicit directory creators; the explicit mkdir tool is the
	// one place directories are made, and this check turns the substrate's
	// opaque "create temp: no such file or directory" into an actionable
	// message.
	parent := wsPathDir(ws, dst)
	if req.MkdirP {
		if err := ws.MkdirAll(ctx, parent, 0o755); err != nil {
			return fmt.Errorf("%s: create destination parent %s: %w", tool, parent, err)
		}
	}
	switch info, statErr := ws.Stat(ctx, parent); {
	case errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("%s: destination parent %s does not exist; create it first with mkdir", tool, parent)
	case statErr != nil:
		return fmt.Errorf("%s: %s: %w", tool, parent, statErr)
	case !info.IsDir():
		return fmt.Errorf("%s: destination parent %s is not a directory", tool, parent)
	}
	return nil
}
