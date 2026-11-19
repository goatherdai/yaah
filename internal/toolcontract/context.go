package toolcontract

import "context"

type toolCallIDKey struct{}

// WithToolCallID attaches the provider tool-call id to the dispatch context.
func WithToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext returns the dispatch tool-call id, or "".
func ToolCallIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}

type toolNameKey struct{}

// WithToolName attaches the executing tool's name to the context.
func WithToolName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, toolNameKey{}, name)
}

// ToolNameFromContext returns the executing tool's name, or "".
func ToolNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(toolNameKey{}).(string)
	return name
}

type supervisionMetaKey struct{}

// WithSupervisionMeta returns a context carrying a metadata sink for one
// tool execution. The caller owns the map and reads it after the call.
func WithSupervisionMeta(ctx context.Context, sink *map[string]any) context.Context {
	return context.WithValue(ctx, supervisionMetaKey{}, sink)
}

// SetSupervisionMeta records one fact on the context's sink. Without a sink
// it is a no-op, so tools never branch on the supervision layer's presence.
func SetSupervisionMeta(ctx context.Context, key string, value any) {
	sink, ok := ctx.Value(supervisionMetaKey{}).(*map[string]any)
	if !ok || sink == nil {
		return
	}
	if *sink == nil {
		*sink = map[string]any{}
	}
	(*sink)[key] = value
}
