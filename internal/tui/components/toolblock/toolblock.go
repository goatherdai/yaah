package toolblock

import (
	"fmt"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/tui/colors"
)

func Icon(name string) string {
	switch name {
	case "read":
		return "📖"
	case "write", "edit", "append":
		return "📝"
	case "delete", "rmdir":
		return "🧹"
	case "patch", "replace":
		return "🩹"
	case "apply_out":
		return "📤"
	case "grep", "glob":
		return "🔍"
	case "find":
		return "🧭"
	case "ls", "file_info":
		return "📂"
	case "mkdir":
		return "📁"
	case "tail":
		return "🪶"
	case "checksum":
		return "🔑"
	case "copy":
		return "👯"
	case "copy_tree":
		return "🌳"
	case "move":
		return "🚚"
	case "move_tree":
		return "🌲"
	case "bulk_files":
		return "📚"
	case "http", "webfetch":
		return "🌐"
	case "mcp":
		return "🔌"
	case "bash":
		return "💻"
	case "powershell":
		return "🪟"
	case "git", "diff", "bisect":
		return "📦"
	case "go_test", "go_outline", "go_refactor", "staticcheck", "go_mod":
		return "🧪"
	case "json_query":
		return "📄"
	case "calculate":
		return "🧮"
	case "todowrite":
		return "✅"
	case "question":
		return "❓"
	case "plan", "list_subagents":
		return "📋"
	case "skill":
		return "🎯"
	case "role":
		return "🎭"
	case "memory_search", "memory_add", "memory_update", "memory_delete", "memory_promote", "memory_search_sessions":
		return "🧠"
	case "background_process":
		return "🌀"
	case "task":
		return "🔗"
	case "subagent_jobs":
		return "📇"
	case "supervise":
		return "📡"
	case "supervisor":
		return "👑"
	case "proposal":
		return "🤝"
	case "toolbelt":
		return "🧰"
	case "wait_for":
		return "⌛"
	case "watch":
		return "👀"
	case "workflow_run":
		return "🚀"
	case "workflow_list":
		return "📃"
	default:
		return "🔧"
	}
}

type State int

const (
	Running State = iota
	Done
	Error
	// Interrupted is terminal without a result: the turn ended before the
	// call's end event arrived (the DoneEvent sweep), so the block is
	// sealed with an explicit marker instead of a permanent chevron.
	Interrupted
)

type Block struct {
	name      string
	icon      string
	args      string
	result    string
	tag       string
	state     State
	expanded  bool
	id        string
	startTime time.Time
	endTime   time.Time
	// durationLabel overrides the computed duration for restored rows,
	// whose timing data was not persisted.
	durationLabel string
	theme         *colors.Theme
}

func New(id, name, args string, th *colors.Theme) *Block {
	icon := Icon(name)
	return &Block{
		id:        id,
		name:      name,
		icon:      icon,
		args:      args,
		state:     Running,
		startTime: time.Now(),
		theme:     th,
	}
}

func (b *Block) ID() string       { return b.id }
func (b *Block) Name() string     { return b.name }
func (b *Block) S() State         { return b.state }
func (b *Block) IsExpanded() bool { return b.expanded }
func (b *Block) Toggle()          { b.expanded = !b.expanded }

// IsRunning reports whether the block is still waiting for its end
// event; the turn-end sweep uses it so callers do not need this
// package's constants.
func (b *Block) IsRunning() bool { return b.state == Running }

// SearchText returns the block's searchable content: the tool name, its
// args, and its result. Search runs over this instead of the rendered
// form so it stays cheap and independent of the paint width.
func (b *Block) SearchText() string { return b.name + " " + b.args + "\n" + b.result }

// SetDurationLabel replaces the computed duration with a fixed label.
// Restored transcript rows use it because their timing was not persisted;
// showing "0ms" would imply a real measurement.
func (b *Block) SetDurationLabel(label string) { b.durationLabel = label }

func (b *Block) Complete(summary, result string) {
	b.endTime = time.Now()
	b.state = Done
	b.tag = summary
	b.result = result
}

func (b *Block) Fail(summary, err string) {
	b.endTime = time.Now()
	b.state = Error
	b.tag = summary
	b.result = err
}

// Interrupt seals a still-Running block whose end event never arrived:
// the turn is over, so no result exists to show — only the fact that the
// call was cut short.
func (b *Block) Interrupt() {
	b.endTime = time.Now()
	b.state = Interrupted
	b.tag = "interrupted"
}

func (b *Block) Render() string { return b.RenderCtx(colors.RenderCtx{Theme: b.theme}) }

func (b *Block) RenderCtx(ctx colors.RenderCtx) string {
	if !b.expanded {
		return b.renderCollapsed()
	}
	return b.renderExpanded(ctx.Width)
}

func (b *Block) durationStr() string {
	if b.durationLabel != "" {
		return b.durationLabel
	}
	d := b.endTime.Sub(b.startTime)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (b *Block) renderCollapsed() string {
	hex := b.theme.ToolHex(b.name)
	switch b.state {
	case Running:
		return fmt.Sprintf(`  %s▶ %s %s%s %s· %s%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag(),
			b.theme.DimTag(), b.args, b.theme.ResetTag())
	case Done:
		return fmt.Sprintf(`  %s✓ %s %s%s %s· %s (%s)%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag(),
			b.theme.DimTag(), b.tag, b.durationStr(), b.theme.ResetTag())
	case Error:
		return fmt.Sprintf(`  %s✗ %s %s%s %s· %s (%s)%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag(), b.theme.DimTag(), b.theme.Tag(b.theme.Error, b.tag), b.durationStr(), b.theme.ResetTag())
	case Interrupted:
		return fmt.Sprintf(`  %s◌ %s %s%s %s· %s (%s, %s)%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag(),
			b.theme.DimTag(), b.args, b.tag, b.durationStr(), b.theme.ResetTag())
	default:
		return ""
	}
}

func (b *Block) renderExpanded(width int) string {
	if width <= 0 {
		width = 58
	}
	hex := b.theme.ToolHex(b.name)
	var bld strings.Builder

	var header string
	switch b.state {
	case Running:
		header = fmt.Sprintf(`  %s▶ %s %s%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag())
	case Done:
		header = fmt.Sprintf(`  %s✓ %s %s%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag())
	case Error:
		header = fmt.Sprintf(`  %s✗ %s %s%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag())
	case Interrupted:
		header = fmt.Sprintf(`  %s◌ %s %s%s`,
			b.theme.ColorTag(hex), b.icon, b.name, b.theme.ResetTag())
	}
	fill := max(2, width-colors.TaggedStringWidth(header)-2)
	bld.WriteString(header)
	bld.WriteString(fmt.Sprintf(`%s %s%s`, b.theme.DimTag(), strings.Repeat("─", fill), b.theme.ResetTag()))
	bld.WriteString("\n")

	if b.args != "" {
		bld.WriteString(fmt.Sprintf(`  %s│%s Args: %s`, b.theme.DimTag(), b.theme.ResetTag(), b.args))
		bld.WriteString("\n")
	}

	if b.state != Running {
		bld.WriteString(fmt.Sprintf(`  %s│%s Duration: %s`, b.theme.DimTag(), b.theme.ResetTag(), b.durationStr()))
		bld.WriteString("\n")
	}

	if b.state == Done && b.result != "" {
		bld.WriteString(fmt.Sprintf(`  %s│────%s`, b.theme.DimTag(), b.theme.ResetTag()))
		bld.WriteString("\n")
		for _, line := range strings.Split(b.result, "\n") {
			if line != "" {
				bld.WriteString(fmt.Sprintf(`  %s│%s %s`, b.theme.DimTag(), b.theme.ResetTag(), line))
			} else {
				bld.WriteString(fmt.Sprintf(`  %s│%s`, b.theme.DimTag(), b.theme.ResetTag()))
			}
			bld.WriteString("\n")
		}
	}

	if b.state == Error && b.result != "" {
		bld.WriteString(fmt.Sprintf(`  %s│%s %s`, b.theme.DimTag(), b.theme.ResetTag(), b.theme.Tag(b.theme.Error, b.result)))
		bld.WriteString("\n")
	}

	if b.state == Interrupted {
		bld.WriteString(fmt.Sprintf(`  %s│%s Interrupted: no result was delivered`, b.theme.DimTag(), b.theme.ResetTag()))
		bld.WriteString("\n")
	}

	bld.WriteString(fmt.Sprintf(`  %s╰%s%s`, b.theme.DimTag(), strings.Repeat("─", width), b.theme.ResetTag()))
	return bld.String()
}
