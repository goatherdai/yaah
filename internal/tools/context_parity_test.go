package tools

import (
	"context"
	"testing"

	"github.com/goatherdai/yaah/internal/toolcontract"
)

// TestContextKeysSharedWithEngine pins the boundary introduced when the
// context helpers moved to toolcontract: the loop writes these keys through
// toolcontract, the tools side reads/writes them through its wrappers, and a
// split key identity would silently drop both ACP tool-call correlation and
// the file-existed facts the advisory rules consume.
func TestContextKeysSharedWithEngine(t *testing.T) {
	t.Run("tool call id", func(t *testing.T) {
		ctx := toolcontract.WithToolCallID(context.Background(), "call-42")
		if got := ToolCallIDFromContext(ctx); got != "call-42" {
			t.Errorf("tools read tool-call id %q, want call-42", got)
		}
	})
	t.Run("tool name", func(t *testing.T) {
		ctx := toolcontract.WithToolName(context.Background(), "bash")
		if got := ToolNameFromContext(ctx); got != "bash" {
			t.Errorf("tools read tool name %q, want bash", got)
		}
	})
	t.Run("supervision metadata", func(t *testing.T) {
		var sink map[string]any
		ctx := toolcontract.WithSupervisionMeta(context.Background(), &sink)
		SetSupervisionMeta(ctx, "file_existed", false)
		if got, ok := sink["file_existed"]; !ok || got != false {
			t.Errorf("supervision sink = %v, want file_existed=false", sink)
		}
	})
}
