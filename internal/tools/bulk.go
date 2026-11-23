package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/prompts"
)

// BulkFilesTool applies one copy/move/delete action to every workspace file
// matching a glob, with a dry-run preview. It exists so relocating or clearing
// a set of files is one call instead of N, especially inside a sub-agent
// sandbox where each turn is expensive.
type BulkFilesTool struct {
	PV *PathValidator
	WS Workspace
}

var (
	_ PathValidatorSetter = (*BulkFilesTool)(nil)
	_ WorkspaceSetter     = (*BulkFilesTool)(nil)
)

func (t *BulkFilesTool) SetPathValidator(pv *PathValidator) { t.PV = pv }
func (t *BulkFilesTool) SetWorkspace(ws Workspace)          { t.WS = ws }

// selfGuardsPath marks bulk_files as taking the per-path lock over the whole
// matched set inside its own Execute (withFileWriteSet, fileguard.go). The
// recording wrapper does not take a lock for it — bulk_files is recorded as an
// unknown mutation, not through the snapshot path — so this marker guards
// against a future wrapper change: any wrapper that acquired the same
// non-reentrant lock first would deadlock the tool on itself.
func (*BulkFilesTool) selfGuardsPath() {}

func (t *BulkFilesTool) Name() string        { return "bulk_files" }
func (t *BulkFilesTool) Description() string { return prompts.ToolDescription("bulk_files") }

func (t *BulkFilesTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["copy", "move", "delete"],
				"description": "Operation to apply to every match."
			},
			"pattern": {
				"type": "string",
				"description": "Glob matched against paths relative to path (e.g. \"*.go\", \"logs/*.jsonl\")."
			},
			"path": {
				"type": "string",
				"description": "Directory to search (default current directory)."
			},
			"to": {
				"type": "string",
				"description": "Destination root for copy/move. The matched path relative to path is preserved under it."
			},
			"force": {
				"type": "boolean",
				"description": "Replace an existing destination file. Default false: it is refused and reported as a failure.",
				"default": false
			},
			"mkdir_p": {
				"type": "boolean",
				"description": "Create missing destination parent directories. Default false.",
				"default": false
			},
			"dry_run": {
				"type": "boolean",
				"description": "List the planned operations without changing anything.",
				"default": false
			}
		},
		"required": ["action", "pattern"]
	}`)
}

func (t *BulkFilesTool) IsDangerous(args string) bool {
	var params struct {
		DryRun bool `json:"dry_run"`
	}
	// A dry run changes nothing, so it does not need approval. A malformed
	// call stays on the conservative side.
	return json.Unmarshal([]byte(args), &params) != nil || !params.DryRun
}

func (t *BulkFilesTool) Execute(ctx context.Context, args string) (string, error) {
	var params struct {
		Action  string `json:"action"`
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		To      string `json:"to"`
		Force   bool   `json:"force"`
		MkdirP  bool   `json:"mkdir_p"`
		DryRun  bool   `json:"dry_run"`
	}
	if err := StrictToolArgs("bulk_files", t.Schema(), args, &params); err != nil {
		return "", err
	}
	switch params.Action {
	case "copy", "move":
		if params.To == "" {
			return "", fmt.Errorf("bulk_files: to is required for %s", params.Action)
		}
	case "delete":
		if params.To != "" {
			return "", fmt.Errorf("bulk_files: to is not valid for delete")
		}
	default:
		return "", fmt.Errorf("bulk_files: action must be copy, move, or delete")
	}
	if params.Pattern == "" {
		return "", fmt.Errorf("bulk_files: pattern is required")
	}
	if params.Path == "" {
		params.Path = "."
	}
	ws := workspaceOf(t.WS, t.PV)
	root, err := ws.ResolveWritePath(params.Path)
	if err != nil {
		return "", fmt.Errorf("bulk_files: %w", err)
	}
	rootInfo, err := ws.Stat(ctx, root)
	if err != nil {
		return "", fmt.Errorf("bulk_files: %s: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("bulk_files: path %s is not a directory", root)
	}

	var toRoot string
	if params.Action != "delete" {
		toRoot, err = ws.ResolveWritePath(params.To)
		if err != nil {
			return "", fmt.Errorf("bulk_files: %w", err)
		}
		if pathWithin(ws, root, toRoot) {
			return "", fmt.Errorf("bulk_files: destination %s is inside the search root %s", toRoot, root)
		}
		if err := refuseSymlinkAncestors(ctx, ws, toRoot); err != nil {
			return "", fmt.Errorf("bulk_files: %w", err)
		}
	}

	re, err := patternToRegex(filepath.ToSlash(params.Pattern))
	if err != nil {
		return "", fmt.Errorf("bulk_files: invalid pattern: %w", err)
	}
	hasSep := strings.ContainsAny(params.Pattern, "/\\")
	var matches []string
	walkErr := walkWorkspace(ctx, ws, root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d == nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		target := filepath.ToSlash(rel)
		if !hasSep {
			target = d.Name()
		}
		if re.MatchString(target) {
			matches = append(matches, p)
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipDir && walkErr != filepath.SkipAll {
		return "", fmt.Errorf("bulk_files: %w", walkErr)
	}
	if len(matches) == 0 {
		return "No matches.", nil
	}
	sort.Strings(matches)

	// A dry run writes nothing, so it must stay lock-free: it cannot contend
	// with a real writer, and holding the guard for a report would make a
	// preview wait behind one.
	if params.DryRun {
		var preview multiResult
		for _, src := range matches {
			rel, err := filepath.Rel(root, src)
			if err != nil {
				preview.failf("%s: %v", src, err)
				continue
			}
			if params.Action == "delete" {
				preview.donef("delete %s", src)
			} else {
				preview.donef("%s %s -> %s", params.Action, src, ws.Join(toRoot, rel))
			}
		}
		return fmt.Sprintf("bulk_files %s dry-run (%d match(es)):%s", params.Action, len(matches), reportList(&preview)), nil
	}

	var res multiResult
	// One acquisition over the whole matched set — every source, plus every
	// resolved destination for copy/move — then the loop below calls the
	// UNLOCKED Workspace primitives, never a guard-routed tool: that is the
	// fileguard.go contract for a multi-path tool (lock once, no re-entry).
	// Without it a bulk call was a bare sequence of renames/copies that a
	// concurrent writer could interleave with, per file.
	bulkErr := withFileWriteSet(bulkLockSet(ws, root, toRoot, matches, params.Action), func() error {
		for _, src := range matches {
			rel, err := filepath.Rel(root, src)
			if err != nil {
				res.failf("%s: %v", src, err)
				continue
			}
			var dst string
			if params.Action != "delete" {
				dst = ws.Join(toRoot, rel)
			}

			switch params.Action {
			case "delete":
				if err := ws.Remove(ctx, src); err != nil {
					res.failf("delete %s: %v", src, err)
				} else {
					res.donef("deleted %s", src)
				}
			case "copy", "move":
				// A symlinked directory under to would redirect the write outside
				// the workspace even though the lexical path looks contained.
				if err := refuseSymlinkComponents(ctx, ws, toRoot, rel); err != nil {
					res.failf("%s %s: %v", params.Action, src, err)
					continue
				}
				resolvedDst, rerr := ws.ResolveWritePath(dst)
				if rerr != nil {
					res.failf("%s %s: %v", params.Action, src, rerr)
					continue
				}
				dst = resolvedDst
				if params.MkdirP {
					if err := ws.MkdirAll(ctx, wsPathDir(ws, dst), 0o755); err != nil {
						res.failf("%s %s: create parent: %v", params.Action, src, err)
						continue
					}
				}
				if dstInfo, derr := ws.Lstat(ctx, dst); derr == nil && !params.Force {
					res.failf("%s %s: destination %s already exists; pass force=true", params.Action, src, dst)
					continue
				} else if derr != nil && !errors.Is(derr, fs.ErrNotExist) {
					res.failf("%s %s: %v", params.Action, src, derr)
					continue
				} else if dstInfo != nil && dstInfo.IsDir() {
					res.failf("%s %s: destination %s is a directory", params.Action, src, dst)
					continue
				}
				var opErr error
				if params.Action == "copy" {
					opErr = ws.Copy(ctx, src, dst)
				} else {
					opErr = ws.Move(ctx, src, dst)
				}
				if opErr != nil {
					res.failf("%s %s -> %s: %v", params.Action, src, dst, opErr)
				} else {
					res.donef("%s %s -> %s", params.Action, src, dst)
				}
			}
		}
		return nil
	})
	if bulkErr != nil {
		return "", fmt.Errorf("bulk_files: %w", bulkErr)
	}
	return res.Report(fmt.Sprintf("bulk_files %s (%d match(es)):", params.Action, len(matches)))
}

// bulkLockSet is the per-path lock set for one bulk call: every matched source,
// plus every resolved destination when the action has one. A destination the
// path layer refuses is left out — the loop reports that refusal per file, and
// locking a path the call cannot write to buys nothing.
func bulkLockSet(ws Workspace, root, toRoot string, matches []string, action string) []string {
	paths := make([]string, 0, len(matches)*2)
	paths = append(paths, matches...)
	if action == "delete" {
		return paths
	}
	for _, src := range matches {
		rel, err := filepath.Rel(root, src)
		if err != nil {
			continue
		}
		if dst, err := ws.ResolveWritePath(ws.Join(toRoot, rel)); err == nil {
			paths = append(paths, dst)
		}
	}
	return paths
}

// reportList renders the success steps for a dry run; the multiResult's
// failure list is empty in that path.
func reportList(r *multiResult) string {
	var b strings.Builder
	for _, d := range r.done {
		b.WriteString("\n  ")
		b.WriteString(d)
	}
	return truncateResult(b.String())
}
