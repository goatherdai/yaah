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
