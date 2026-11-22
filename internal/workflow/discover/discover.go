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
// dir (<cwd>/workflows), then the global dir (<yaah home>/workflows). Earlier
// directories win name collisions.
func SearchDirs(cwd string, extra ...string) []string {
	dirs := make([]string, 0, len(extra)+2)
	dirs = append(dirs, extra...)
	if cwd != "" {
		dirs = append(dirs, filepath.Join(cwd, "workflows"))
	}
	dirs = append(dirs, filepath.Join(config.HomeDir(), "workflows"))
	return dirs
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
			if e.IsDir() {
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
			out = append(out, Spec{
				Name: name,
				Path: filepath.Join(dir, e.Name()),
				Dir:  dir,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
		for _, ext := range specExts {
			p := filepath.Join(dir, ref+ext)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
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
