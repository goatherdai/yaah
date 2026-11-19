package tools

import (
	"context"

	"github.com/goatherdai/yaah/internal/toolcontract"
)

// Supervision metadata is tool-observed context the advisory rules read
// from a declaration's payload. The key lives in toolcontract so the sink the
// loop installs and the facts the write tools record share one identity.

func WithSupervisionMeta(ctx context.Context, sink *map[string]any) context.Context {
	return toolcontract.WithSupervisionMeta(ctx, sink)
}

func SetSupervisionMeta(ctx context.Context, key string, value any) {
	toolcontract.SetSupervisionMeta(ctx, key, value)
}
