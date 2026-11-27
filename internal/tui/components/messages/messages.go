// Package messages defines the conversation item type and emits its
// rendered tagged form. The transcript painter wraps these emissions
// into screen rows; emission is the single source shared by painting,
// row counting, and text extraction.
package messages

import (
	"strings"

	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/reasoning"
	"github.com/goatherdai/yaah/internal/tui/components/toolblock"
)

// Item is a single entry in the chronological conversation log.
// Exactly one of the fields is set — Text for plain/markdown content,
// ToolBlock for tool calls, or ReasBlock for reasoning sections.
// Sub-agents are not conversation items; they render in their own pane.
type Item struct {
	Text      string
	ToolBlock *toolblock.Block
	ReasBlock *reasoning.Block
}

// Content carries rendering context.
type Content struct {
	Width int
	Theme *colors.Theme
}

// EmitItem appends the rendered form of a single item to b. One item's
// emission is a self-contained tagged string: text items open with a
// separator newline and close with the blank line that hands the next
// item its own leading row, and block items carry their own layout.
func EmitItem(b *strings.Builder, item *Item, rctx colors.RenderCtx) {
	switch {
	case item.Text != "":
		b.WriteString("\n")
		b.WriteString(item.Text)
		b.WriteString("\n\n")
	case item.ToolBlock != nil:
		b.WriteString(item.ToolBlock.RenderCtx(rctx))
		b.WriteString("\n")
	case item.ReasBlock != nil:
		b.WriteString("\n")
		b.WriteString(item.ReasBlock.RenderCtx(rctx))
		b.WriteString("\n\n")
	}
}

// Render returns item's emitted tagged form as a string.
func Render(item Item, rctx colors.RenderCtx) string {
	var b strings.Builder
	EmitItem(&b, &item, rctx)
	return b.String()
}
