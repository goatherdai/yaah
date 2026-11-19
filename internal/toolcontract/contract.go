// Package toolcontract holds the small vocabulary both the engine and the
// built-in tool surface need, without either importing the other. Tool and
// DangerClassifier are structural contracts, so aliasing them on both sides
// keeps a concrete *tools.Registry assignable to the engine's ToolRegistry
// port: Go requires identical method signatures, not merely identical method
// sets, for interface satisfaction.
package toolcontract

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/goatherdai/yaah/internal/conflictapi"
)

// Tool is the interface every built-in and MCP tool satisfies.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Execute(ctx context.Context, args string) (string, error)
}

// DangerClassifier is the optional per-tool interface the approval gate
// consults for argument-level danger.
type DangerClassifier interface {
	IsDangerous(argsJSON string) bool
}

// ConflictDetector is the engine's view of the post-dispatch conflict
// tracker. The report type is neutral so the loop and its pipeline can read
// conflicts without importing the built-in tool surface.
type ConflictDetector interface {
	DetectAndResetReport() *conflictapi.ConflictReport
}

// IsNil reports whether an interface-valued dependency is absent, including
// a typed-nil pointer stored in it. Ports are satisfied by concrete pointers,
// so a nil *tools.ConflictTracker assigned to a ConflictDetector is a non-nil
// interface that would otherwise pass a plain == nil check.
func IsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}
