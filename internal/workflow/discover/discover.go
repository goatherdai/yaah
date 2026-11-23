// Package discover locates workflow documents in the directories a run
// resolves bare names against (workflow default-location feature). It is the
// single source of the search order: caller-supplied directories first, then
// the workspace's workflows/ dir, then the global ~/.yaah/workflows dir, so
// every surface (CLI, TUI, and the workflow tools) lists and resolves names
// against the same set.
package discover

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goatherdai/yaah/internal/config"
)

// Spec is one discovered workflow document.
type Spec struct {
	// Name is the base name without extension — the handle `run <name>`
	// resolves.
	Name string `json:"name"`
	// Path is the absolute path to the document.
	Path string `json:"path"`
	// Dir is the search directory the document was found in.
	Dir string `json:"dir"`
}

// specExts are the document extensions a workflow may use.
var specExts = []string{".yaml", ".yml", ".json"}

// SearchDirs returns the ordered directories scanned for workflow documents:
// extra dirs first (caller-supplied, e.g. --workflow-dir), then the workspace
// dirs walking up from cwd to the filesystem root (<cwd>/workflows, then each
// existing <ancestor>/workflows, nearest first), then the global dir
// (<yaah home>/workflows). Earlier directories win name collisions.
func SearchDirs(cwd string, extra ...string) []string {
	dirs := make([]string, 0, len(extra)+4)
	for _, d := range extra {
		dirs = append(dirs, absDir(d))
	}
	dirs = append(dirs, projectDirs(cwd)...)
	dirs = append(dirs, filepath.Join(config.HomeDir(), "workflows"))
	return dirs
}

// projectDirs returns the workspace search dirs for cwd: <cwd>/workflows is
// always present (so the workspace tier exists even before a workflows dir is
// created), followed by each existing <ancestor>/workflows walking up to the
// filesystem root, nearest first. Walking up lets `yaah workflow run name`
// resolve a project's workflows/ from any subdirectory of that project.
func projectDirs(cwd string) []string {
	cwd = absDir(cwd)
	if cwd == "" {
		return nil
	}
	out := []string{filepath.Join(cwd, "workflows")}
	cur := filepath.Dir(cwd)
	for cur != cwd {
		d := filepath.Join(cur, "workflows")
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
		cwd = cur
		cur = filepath.Dir(cwd)
	}
	return out
}

// absDir makes a caller-supplied directory absolute (resolved against the
// process working directory). A relative --workflow-dir would otherwise leak
// relative paths into every listed Spec, which the caller then fails to open
// from a different directory.
func absDir(dir string) string {
	if dir == "" {
		return dir
	}
	if a, err := filepath.Abs(dir); err == nil {
		return a
	}
	return dir
}

// List scans SearchDirs and returns the discovered specs, deduplicated by
// name (the first directory in the order wins) and sorted by name. A search
// directory that does not exist is skipped; one that exists but cannot be
// read is an error.
func List(cwd string, extra ...string) ([]Spec, error) {
	seen := make(map[string]bool)
	var out []Spec
	for _, dir := range SearchDirs(cwd, extra...) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("workflow/discover: read %s: %w", dir, err)
		}
		for _, e := range entries {
			// Re-stat rather than trusting DirEntry.IsDir(), which does not
			// follow a symlink: a symlink named foo.yaml pointing at a
			// directory is not a runnable spec, and Resolve (which uses
			// os.Stat) rejects it, so List must too.
			if fi, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || fi.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if !isSpecExt(ext) {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ext)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			// specInDir picks the same file Resolve would (extension order),
			// so the listed path is the one that actually runs.
			out = append(out, Spec{
				Name: name,
				Path: specInDir(dir, name),
				Dir:  dir,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// specInDir returns the document path Resolve would select for name in dir:
// the first of <name>.yaml, <name>.yml, <name>.json that exists as a regular
// file. List and Resolve share it so listing and running agree.
func specInDir(dir, name string) string {
	for _, ext := range specExts {
		p := filepath.Join(dir, name+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// Resolve resolves a run reference to a document path. A reference that
// carries a path separator or a spec extension is a path, returned resolved
// against cwd (existence is the caller's concern, so a typo fails where it is
// used). Anything else is a bare name, resolved as <name>.yaml, <name>.yml,
// or <name>.json across SearchDirs, first match wins.
func Resolve(cwd, ref string, extra ...string) (string, error) {
	if filepath.IsAbs(ref) || strings.ContainsAny(ref, `/\`) || isSpecExt(filepath.Ext(ref)) {
		if filepath.IsAbs(ref) {
			return ref, nil
		}
		return filepath.Join(cwd, ref), nil
	}
	for _, dir := range SearchDirs(cwd, extra...) {
		if p := specInDir(dir, ref); p != "" {
			return p, nil
		}
	}
	return "", &NotFoundError{Name: ref, Dirs: SearchDirs(cwd, extra...)}
}

// NotFoundError reports that a bare name matched no document in the search
// directories.
type NotFoundError struct {
	Name string
	Dirs []string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("workflow %q not found; searched %s", e.Name, strings.Join(e.Dirs, ", "))
}

func isSpecExt(ext string) bool {
	for _, s := range specExts {
		if ext == s {
			return true
		}
	}
	return false
}
