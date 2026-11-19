package agent

import (
	"context"
	"reflect"

	"github.com/goatherdai/yaah/internal/toolcontract"
)

// Tool and DangerClassifier are the engine's names for the neutral tool
// contracts. They are aliases, not fresh interfaces: a concrete
// *tools.Registry returns toolcontract.Tool from Get, and Go requires the
// identical type for its method signature to satisfy ToolRegistry below.
type Tool = toolcontract.Tool
type DangerClassifier = toolcontract.DangerClassifier

// ToolRegistry is the port over the tool surface the loop dispatches through.
// internal/tools.Registry satisfies it; a test or an alternate host can supply
// a fake without linking the built-in tools.
type ToolRegistry interface {
	Get(name string) Tool
	List() []string
	Execute(ctx context.Context, name, args string) (string, error)
	Generation() int
}

// registryNil reports whether the registry is absent. A nil *tools.Registry
// stored in a ToolRegistry is a non-nil interface, so a plain `== nil` check
// misses it and the tool-definition and plan-mode listing paths panic. This
// keeps the no-registry behavior the concrete pointer field had.
func registryNil(r ToolRegistry) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// normalizeRegistry folds a typed-nil registry to a nil interface so every
// consumer, including dispatch paths with no explicit guard, sees the same
// absence the loop saw before the port was introduced.
func normalizeRegistry(r ToolRegistry) ToolRegistry {
	if registryNil(r) {
		return nil
	}
	return r
}
