package tools

import (
	"context"

	"github.com/goatherdai/yaah/internal/toolcontract"
)

// The tool-call and tool-name context keys live in toolcontract so the agent
// dispatch path and the tools side share one key identity. These wrappers keep
// the tools package's public surface unchanged.

func WithToolCallID(ctx context.Context, id string) context.Context {
	return toolcontract.WithToolCallID(ctx, id)
}

func ToolCallIDFromContext(ctx context.Context) string {
	return toolcontract.ToolCallIDFromContext(ctx)
}

func WithToolName(ctx context.Context, name string) context.Context {
	return toolcontract.WithToolName(ctx, name)
}

func ToolNameFromContext(ctx context.Context) string {
	return toolcontract.ToolNameFromContext(ctx)
}
