// Package control defines the control-plane message types sent from
// tool handlers and session infrastructure to a UI consumer (TUI, REPL,
// web, ACP). These are distinct from the provider wire-format types in
// internal/types — they carry UI-level concerns (questions, approvals,
// status updates, todo lists, context info) rather than LLM request/response
// shapes.
//
// Extracting these from internal/types breaks the types → todo dependency,
// allowing the provider wire-format package to remain dependency-free.
//
// bridge.go, question.go and approval.go carry the delivery policy those
// consumers share — how long a prompt may take to be accepted, how long the
// user may take to answer it, and what a surface that cannot ask falls back to
// — so the TUI, the web UI and ACP cannot each re-derive (and drift from) it.
package control

import (
	"fmt"

	"github.com/goatherdai/yaah/internal/todo"
)

// Msg is the sealed interface for control-plane messages sent from
// tool handlers and session infrastructure to a UI consumer (TUI, REPL, etc.).
// Each concrete type implements marker().
type Msg interface {
	marker()
}

// Status carries a notification string for display.
type Status struct{ Text string }

func (*Status) marker() {}

// CompactionSummary carries the summary a completed compaction produced so
// surfaces can render it in the conversation stream (yaah-v378.5). It is
// render-only: the summary already lives in the system message, so it must
// never be persisted as a chat message. Summary is empty when every
// summarizer attempt failed and the pass fell back to trimming.
type CompactionSummary struct {
	Summary string
	Method  string // "single", "chunked", or "trim"
	Before  int
	After   int
	Window  int
}

func (*CompactionSummary) marker() {}

// Header renders the one-line outcome shared by every surface: which path
// ran and how the token count moved. Surfaces append the summary (or their
// own no-summary notice) after it.
func (m *CompactionSummary) Header() string {
	return fmt.Sprintf("context compacted (%s): %d → %d tokens", m.Method, m.Before, m.After)
}

// WorkflowRunReport carries a completed :workflow run report so surfaces can
// render it in the conversation stream. Render-only, like CompactionSummary:
// the report already lives in the run record, so it must never be persisted
// as a chat message.
type WorkflowRunReport struct {
	Path   string
	OK     bool
	Report string // rendered body (markdown)
}

func (*WorkflowRunReport) marker() {}

// WorkflowProgress carries one live node/run transition from a workflow
// executing inside the session (workflow-in-session Scope B). Unlike
// WorkflowRunReport, which reports once at completion, this fires as the run
// advances so a surface can show per-node progress without waiting for the
// final report.
type WorkflowProgress struct {
	// NodePath is the node this fact is about; empty for run-level facts
	// (State "started" or "finished").
	NodePath string
	// State is one of the small vocabulary a view renders directly:
	// "started" (run-level), "queued", "running", "succeeded", "failed",
	// "skipped", "cancelled", or "finished" (run-level).
	State string
	// Attempt is the 1-based attempt ordinal when State is "running"; 0
	// otherwise.
	Attempt int
	// Reason is the terminal fact's cause (failed/skipped/cancelled); empty
	// otherwise.
	Reason string
}

func (*WorkflowProgress) marker() {}

// Error carries an error for display.
type Error struct{ Err error }

func (*Error) marker() {}

// Option is a single choice in a question or approval prompt.
type Option struct {
	Label       string
	Description string
}

// Question carries an interactive question to display.
type Question struct {
	Header   string
	Question string
	Options  []Option
	Multiple bool
	AnswerCh chan<- string
}

func (*Question) marker() {}

// Approval carries a tool approval prompt.
type Approval struct {
	// CallID is the provider's tool_call id for the call awaiting a decision,
	// so a surface can correlate the prompt with the tool row it belongs to.
	// Empty when the ask carries no call (an out-of-workspace path decision).
	CallID    string
	Name      string
	Args      string
	ApproveCh chan<- bool
}

func (*Approval) marker() {}

// Continue asks the host to prompt whether to continue after reaching
// the maximum number of agent iterations. Write true (continue) or false
// (stop) to AnswerCh.
type Continue struct {
	MaxIter  int
	AnswerCh chan<- bool
}

func (*Continue) marker() {}

// ModelList carries the set of available model identifiers.
type ModelList struct {
	Models        []string
	ProviderNames map[string]string
}

func (*ModelList) marker() {}

// Todos carries an updated todo list.
type Todos struct {
	Items []todo.Item
}

func (*Todos) marker() {}

// ContextInfo carries context window usage statistics.
type ContextInfo struct {
	Tokens                    int
	Window                    int
	LastPromptTokens          int // provider-shaped prompt tokens (last turn)
	LastPromptTokensInclusive int // cache-inclusive provider prompt size (last turn)
}

func (*ContextInfo) marker() {}

// Fallback is sent when the LLM client falls back to an alternative
// provider. The TUI should update its header to reflect the new provider.
type Fallback struct {
	Provider string
	Model    string
}

func (*Fallback) marker() {}

// Usage is a token-count snapshot for one side of the usage split. It is a
// local type so the control protocol stays independent of the provider
// wire-format types in internal/types (see the package doc).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
}

// UsageInfo carries the session-cumulative usage split so a view can render
// orchestrator and sub-agent spend separately. Agent is the orchestrator's
// own spend (the session total minus the sub-agent share); Sub is the
// sub-agent share (foreground and background dispatches), already folded
// into the session totals.
type UsageInfo struct {
	Agent Usage
	Sub   Usage
}

func (*UsageInfo) marker() {}

// Done is a sentinel sent once when the session closes.
// The goroutine forwarding from controlCh to prog.Send must detect
// this type and return to avoid a leaked goroutine after the TUI exits.
type Done struct{}

func (*Done) marker() {}
