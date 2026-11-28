package tui

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/goatherdai/yaah/internal/control"
	"github.com/goatherdai/yaah/internal/todo"
	"github.com/goatherdai/yaah/internal/tui/colors"
	"github.com/goatherdai/yaah/internal/tui/components/activity"
	"github.com/goatherdai/yaah/internal/tui/components/command"
	"github.com/goatherdai/yaah/internal/tui/components/input"
	"github.com/goatherdai/yaah/internal/tui/components/mcpinfo"
	"github.com/goatherdai/yaah/internal/tui/components/rule"
	"github.com/goatherdai/yaah/internal/tui/components/subagent"
	"github.com/goatherdai/yaah/internal/tui/transcript"
	"github.com/goatherdai/yaah/internal/worldinfo"
	"github.com/rivo/tview"
)

// DriverHooks groups everything the driver (cmd/yaah/tui.go) injects
// into the TUI: submit/steer/follow-up/stop seams, the workflow
// launcher, the model/agent/mode pickers' selection sinks, and the
// control-plane channel. They are one struct because they are one
// contract — none of them is state the TUI derives; they all hand a
// user intent to the session driver. Nil callbacks mean the driver did
// not wire that surface; every call site treats that as "unavailable
// in this session", never as a crash.
type DriverHooks struct {
	OnSubmit   func(prompt string)
	OnAbort    func()
	OnCompact  func()
	OnClear    func()
	OnSteer    func(text string)
	OnFollowUp func(text string)
	OnStop     func()

	// OnWorkflowRun receives a spec file path the user typed at
	// :workflow run; OnWorkflowList is invoked at :workflow list; and
	// OnWorkflowPicker when :workflow run is given no spec, where the
	// driver discovers the specs and opens the selector modal
	// (ShowWorkflowPicker). Bare :workflow prints usage (RequiresArg).
	OnWorkflowRun    func(specPath string)
	OnWorkflowList   func()
	OnWorkflowPicker func()

	OnModelSelect func(model string)
	// OnAgentSelect receives the persona the user picked in the :agent
	// palette. The driver validates it and switches the current
	// session to it (SetPersona).
	OnAgentSelect func(name string)
	// OnModeSelect receives the mode word the user picked in the :mode
	// picker (read-only or full). The driver switches this session's
	// mode axis, effective at the next run.
	OnModeSelect func(word string)
	// OnRolesReload is invoked at :roles (REPL /roles parity): the
	// driver hot-reloads the role registry from the session's search
	// paths and reports the outcome through the control channel.
	OnRolesReload func()
	// OnLogin and OnLogout receive the OAuth provider name typed at
	// :login / :logout, "" when none was. The driver resolves "": the
	// single configured provider is picked automatically; multiple
	// configured providers without a name are refused with the names
	// listed, since the TUI has no stdin to prompt on. The device-flow
	// status messages flow back through the control channel.
	OnLogin  func(provider string)
	OnLogout func(provider string)

	// ControlCh is the typed control-plane message channel (todos,
	// questions, approvals, usage); the driver creates it and consumes
	// its view side.
	ControlCh <-chan control.Msg
}

// App is the tview-based terminal UI prototype.
//
// Concurrency: fields are grouped into three categories:
//   - Set before Run() — safe to read from any goroutine after setup
//   - QueueUpdateDraw only — MUST only be accessed inside QueueUpdateDraw/QueueUpdate callbacks
//   - Atomic / immutable — safe to access from any goroutine
type App struct {
	App   *tview.Application
	Pages *tview.Pages
	Root  *tview.Flex

	Theme *colors.Theme

	// Hooks is the driver contract; see DriverHooks.
	Hooks DriverHooks

	// Transcript: tr owns the conversation log and every derived
	// painting table (window, rows, anchors); pane paints tr's rows
	// into its rect and owns the viewport offset. Both are
	// QueueUpdateDraw-only state, with the documented pre-Run seeding
	// exception (see transcript.Model).
	tr   *transcript.Model
	pane *transcript.Pane

	// scrollDrag is the scrollbar thumb's grab distance (pointer row
	// minus thumb top) while a thumb drag is active; negative when no
	// drag is in progress. UI-goroutine only (scrollbar_input.go).
	scrollDrag int

	Banner     *tview.TextView
	Input      *tview.TextArea
	promptEcho *tview.TextView
	// promptRule is the separator line under the sticky prompt pin; it
	// shows only while the pin does (see SetCurrentPrompt and
	// reconcilePromptPin).
	promptRule *rule.Rule
	prompt     *input.Prompt

	// bannerLines is the banner TextView's fixed line count (figlet art
	// plus tagline); headerHeight derives the header's grid size from
	// it, padding row included.
	bannerLines int

	// Typeahead: suggestionsView is a strip above the composer that appears
	// while a colon command name is being typed. suggestionIdx highlights the
	// match Tab will accept; Up/Down cycle, Esc dismisses. Touched only from the
	// UI goroutine — the composer's changed handler and the key capture.
	suggestionsView    *tview.TextView
	suggestions        []command.Completion
	suggestionIdx      int
	screenHeight       int // last drawn screen height; zero until first draw
	messagesCol        *tview.Flex
	InfoPane           *tview.TextView
	TodoPane           *tview.TextView
	BackgroundJobsPane *tview.TextView
	rightPane          *tview.Flex
	Header             *tview.Grid

	McpServers []mcpinfo.Server

	// --- QueueUpdateDraw ONLY fields below ---
	//
	// Exception: the history seed mutates the transcript model
	// directly on the setup goroutine before App.Run starts. No queued
	// callback executes until App.Run drains them, so the seed cannot
	// race with a live event.

	activityLine *activity.Row
	focus        focusState
	// modalDepth counts open layered views so nested overlays release focus in
	// order; it is paired with focus and touched only from the UI goroutine.
	modalDepth int

	// agentUsage and subUsage carry the session-cumulative usage split
	// pushed via control.UsageInfo. agentUsage is the orchestrator's own
	// spend; subUsage is the sub-agent share (foreground + background).
	agentUsage control.Usage
	subUsage   control.Usage

	// Protects pendingTokens during concurrent writes from the
	// forwarder goroutine (HandleEvent) and reads on the main
	// thread (flushPendingTokens).
	tokenMu       sync.Mutex
	pendingTokens strings.Builder

	// ShowApprovalFn overrides the approval modal for testing.
	// If nil (default), the tview-based approval.Show modal is used.
	ShowApprovalFn func(name, args string, onAnswer func(bool))

	bgMu      sync.Mutex
	bgDone    chan struct{}
	uiEventCh chan uiEvent

	coalesceMu           sync.Mutex
	thinkingQueued       bool
	thinkingSeq          uint64
	pendingThinkingLabel string
	contextQueued        bool
	contextSeq           uint64
	pendingContextTokens int
	pendingContextWindow int

	pendingTool     string
	compacting      bool
	compactBoundary int
	contextTokens   int
	contextWindow   int
	lastProvider    string
	lastModel       string
	lastMode        string
	todoItems       []todo.Item
	verbose         bool
	showBanner      bool
	ephemeralMsg    string
	// ephemeralGen guards SetEphemeral: each call invalidates the
	// previous timer so a stale clear cannot stomp a newer message.
	ephemeralGen         atomic.Int64
	subAgentsEnabled     bool
	subAgentsProvider    string
	subAgentsConcurrency int
	subAgentsModel       string
	embeddingEnabled     bool
	embeddingModel       string
	worldStatus          worldinfo.Status
	agentActive          bool
	// stopArmedAt is the instant the first Esc of a stop arm was
	// pressed (see stop.go); the zero time is disarmed.
	stopArmedAt time.Time
	// stopEchoFlushed reports that the stop echo's flush painted the
	// interrupted partial, so the turn's DoneEvent must not render
	// DoneEvent.Response over it a second time (see stop.go EchoStop).
	// Cleared at DoneEvent with stopArmedAt.
	stopEchoFlushed     atomic.Bool
	subAgents           []*subagent.Block
	subAgentsBusy       atomic.Bool
	activityBusy        atomic.Bool
	tokensRx            atomic.Int64
	charsWritten        atomic.Int64
	uiEventDrops        atomic.Int64
	uiEventFallbacks    atomic.Int64
	uiEventFallbackSat  atomic.Int64
	lastRefreshUnixNano atomic.Int64
	// renderMinDelay is the adaptive throttle (see requestRefresh),
	// written by refreshMessages on the UI goroutine.
	renderMinDelay atomic.Int64

	// slowLog persists slow-render diagnostics; stderr is silenced while
	// the TUI is on the alt screen (see slowlog.go). slowThreshold is the
	// refresh duration above which a SLOW line is recorded.
	slowLog       slowLogger
	slowThreshold time.Duration

	isStreaming   atomic.Bool
	needsRefresh  atomic.Bool
	refreshQueued atomic.Bool

	fallbackSem     chan struct{}
	fallbackWaiters chan struct{}

	availableModels []string
	providerNames   map[string]string

	// agentNames/agentDescs back the :agent persona picker; the driver
	// installs them once at startup — the persona set is static per
	// process (role reloads are followed by a restart of the picker
	// data on the next launch).
	agentNames []string
	agentDescs map[string]string

	// Sticky prompt-header state (QueueUpdateDraw ONLY). pinnedPrompt
	// caches what the echo last showed so unchanged pins cost one
	// binary search per draw (see reconcilePromptPin).
	pinnedPrompt string

	version string

	// initialPrompt is a prompt submitted through OnSubmit once the event
	// loop starts. It is the launcher seam for `yaah tui workflow run
	// <spec>` (workflow-in-session Scope B, piece C): set via
	// SetInitialPrompt before Run, read on the Run goroutine.
	initialPrompt string

	// history is the restored conversation seeded before Run. Consumed by
	// restoreHistory on the setup goroutine, then cleared; see history.go.
	history []HistoryItem
}

// SetInitialPrompt queues a prompt for submission through OnSubmit once the
// event loop starts. It is the launcher seam for `yaah tui workflow run
// <spec>` (workflow-in-session Scope B, piece C): set it before Run, and the
// seeded prompt runs through the exact same path a typed prompt does.
func (t *App) SetInitialPrompt(text string) {
	t.initialPrompt = text
}

func New(version string) *App {
	th := colors.DetectTheme()
	t := &App{
		App:             tview.NewApplication(),
		Theme:           &th,
		version:         version,
		showBanner:      true,
		fallbackSem:     make(chan struct{}, uiMaxDirectFallbacks),
		fallbackWaiters: make(chan struct{}, uiMaxDirectWaiters),
		slowThreshold:   50 * time.Millisecond,
	}
	t.tr = transcript.NewModel(t.Theme)
	t.pane = transcript.NewPane(t.tr)
	// Scrollbar colors follow the theme; NO_COLOR keeps the terminal's
	// default foreground so the chrome stays visible but unstyled.
	track, thumb := tcell.ColorDefault, tcell.ColorDefault
	if !t.Theme.NoColor {
		track = tcell.GetColor(t.Theme.Dim)
		thumb = tcell.GetColor(t.Theme.Heading)
	}
	t.pane.SetScrollbarColors(track, thumb)
	t.scrollDrag = -1
	t.buildUI()
	return t
}

// AcceptsEventsBetweenRuns reports that HandleEvent is safe to call from the
// background-job goroutine: it marshals every mutation onto the tview event
// queue rather than writing to the terminal directly.
func (t *App) AcceptsEventsBetweenRuns() bool { return true }

// FlushSlowLog waits for queued slow-render diagnostics to land on disk.
// The driver calls it on shutdown, before stderr is restored from the
// alt-screen silencing.
func (t *App) FlushSlowLog() { t.slowLog.flush() }

func (t *App) SetProvider(name string) {
	t.lastProvider = name
	t.renderInfoPane()
}

func (t *App) SetModel(name string) {
	t.lastModel = name
	t.renderInfoPane()
}

// SetMode updates the info pane's session-mode line.
func (t *App) SetMode(mode string) {
	t.lastMode = mode
	t.renderInfoPane()
}

// SetMCPServers populates the info pane's MCP server list. Must be
// called before Run(); McpServers is read by renderInfoPane from the
// UI thread.
func (t *App) SetMCPServers(servers []mcpinfo.Server) {
	t.McpServers = servers
	t.renderInfoPane()
}

// SetWorld populates the info pane's World section. Must be called before
// Run(); the status is read by renderInfoPane from the UI thread.
func (t *App) SetWorld(s worldinfo.Status) {
	t.worldStatus = s
	t.renderInfoPane()
}

func (t *App) SetConfig(subAgentsEnabled bool, subAgentsProvider string, subAgentsConcurrency int, subAgentsModel string, embeddingEnabled bool, embeddingModel string) {
	t.subAgentsEnabled = subAgentsEnabled
	t.subAgentsProvider = subAgentsProvider
	t.subAgentsConcurrency = subAgentsConcurrency
	t.subAgentsModel = subAgentsModel
	t.embeddingEnabled = embeddingEnabled
	t.embeddingModel = embeddingModel
	t.renderInfoPane()
}
