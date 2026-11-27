package acpclient

import (
	"sort"
	"sync"

	"github.com/goatherdai/yaah/internal/jobs"
)

// Registry resolves an ACP agent name to a sub-agent runner. It is constructed
// once per process from the configured agent manifests; names that are not
// declared resolve to nil so callers can fail closed.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]*Runner
}

// NewRegistry builds a registry from named manifests.
func NewRegistry(manifests map[string]Manifest) *Registry {
	r := &Registry{runners: make(map[string]*Runner, len(manifests))}
	for name, manifest := range manifests {
		r.runners[name] = New(manifest)
	}
	return r
}

// Resolve returns the runner for name, or nil when the name is not declared.
func (r *Registry) Resolve(name string) jobs.TaskRunner {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	runner := r.runners[name]
	if runner == nil {
		return nil
	}
	return runner.TaskRunner()
}

// Names returns the declared names, sorted for stable diagnostics.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runners))
	for name := range r.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
