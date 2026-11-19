package agent

import (
	"context"

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
