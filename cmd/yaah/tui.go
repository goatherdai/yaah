package yaah

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/control"
	"github.com/goatherdai/yaah/internal/tui"
	"github.com/goatherdai/yaah/internal/tui/components/mcpinfo"
	"github.com/goatherdai/yaah/internal/tui/components/workflowpicker"
	"github.com/goatherdai/yaah/internal/workflow/discover"
	"github.com/spf13/cobra"
)

// Removed with the bubbletea TUI: the TUI session is no longer exposed as
// an MCP server. The flags remain (hidden) as deprecation stubs so scripts
// get a migration hint instead of "unknown flag".
var (
	tuiMCPDeprecated     bool
	tuiMCPHTTPDeprecated string
)

func init() {
	rootCmd.AddCommand(tuiCmd)
	tuiCmd.Flags().BoolVar(&tuiMCPDeprecated, "mcp", false, "removed: use `yaah serve` for MCP over stdio")
	tuiCmd.Flags().StringVar(&tuiMCPHTTPDeprecated, "mcp-http", "", "removed: use `yaah serve --http <addr>`")
	_ = tuiCmd.Flags().MarkHidden("mcp")
	_ = tuiCmd.Flags().MarkHidden("mcp-http")

	tuiCmd.AddCommand(tuiWorkflowCmd)
	tuiWorkflowCmd.AddCommand(tuiWorkflowRunCmd)
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the terminal UI",
	Long:  `Launch the interactive terminal UI with rich chat display, streaming, and tool call visualization.`,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseDeprecatedTUIFlags(); err != nil {
			return err
		}
		return runTUI("")
	},
}

// tuiWorkflowCmd groups the TUI workflow launchers (workflow-in-session Scope
// B, piece C). The run subcommand is the CLI form of Scope A's :workflow run:
// it seeds a session that invokes the workflow_run tool as its first action
// instead of the fire-and-forget command path.
var tuiWorkflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Workflow operations for a TUI session",
}

var tuiWorkflowRunCmd = &cobra.Command{
	Use:   "run <spec>",
	Short: "Launch a TUI session seeded to run a workflow",
	Long: `Launch the interactive terminal UI with the session seeded to run the
workflow at <spec> as its first action, through the workflow_run tool.

This is the in-session form of "yaah workflow run": the workflow executes
inside the orchestrator session, so its per-node progress streams into the TUI
and its report lands in the conversation.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := refuseDeprecatedTUIFlags(); err != nil {
			return err
		}
		return runTUI(args[0])
	},
}

// refuseDeprecatedTUIFlags refuses the removed --mcp/--mcp-http flags on any
// TUI entry point. The refusal lives here so the plain tui command and its
// subcommands share one answer.
func refuseDeprecatedTUIFlags() error {
	if tuiMCPDeprecated {
		return fmt.Errorf("tui --mcp was removed; use `yaah serve` for MCP over stdio")
	}
	if tuiMCPHTTPDeprecated != "" {
		return fmt.Errorf("tui --mcp-http was removed; use `yaah serve --http %s`", tuiMCPHTTPDeprecated)
	}
	return nil
}

func runTUI(initialSpec string) error {
	// Interactive surface: honor bare --resume before building the
	// session (stderr is hijacked later; this runs on the real terminal).
	if err := maybeResolveResumePicker(os.Stdout, os.Stdin); err != nil {
		return fmt.Errorf("resume: %v", err)
	}
	sess, err := newAgentSession()
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer sess.close()
	// A termination signal never reaches the deferred close — the process dies
	// where it stands, leaving the session row open. The funnel closes the
	// session first, then exits. Installed before the TUI takes the terminal:
	// in raw mode Ctrl+C is a key event (tview's quit), so the funnel only
	// fires for the signals that would otherwise bypass the close entirely.
	defer sess.CloseOnInterrupt()()

	controlCh := make(chan control.Msg, 64)
	sess.SetCtrlCh(controlCh)

	app := tui.New(version)

	// A resumed session carries its transcript in the agent, not in the
	// TUI's live event stream. Seed the conversation pane from the same
	// History() ACP replays on session/load so resuming lands the user back
	// in the conversation instead of an empty pane.
	app.SetHistory(tuiHistoryFromMessages(sess.History()))

	app.SetProvider(sess.ProviderName())
	app.SetModel(sess.ModelName())
	app.SetMode(sess.Mode())

	infos := sess.MCPInfos()
	mcpServers := make([]mcpinfo.Server, 0, len(infos))
	for _, info := range infos {
		mcpServers = append(mcpServers, mcpinfo.Server{
			Name:      info.Name,
			Connected: info.Connected,
		})
	}
	app.SetMCPServers(mcpServers)

	cfg := sess.cfg
	subModel := cfg.Agent.SubAgent.Model
	if subModel == "" {
		subModel = cfg.Agent.Default.Model
	}
	subProvider := cfg.Agent.SubAgent.Provider
	if subProvider == "" {
		subProvider = sess.ProviderName()
	}
	app.SetConfig(
		cfg.Agent.SubAgent.Provider != "" || cfg.Agent.SubAgent.Model != "",
		subProvider,
		cfg.Agent.SubAgent.MaxConcurrency,
		subModel,
		cfg.EmbeddingSearchLive(),
		cfg.Embedding.Model,
	)
	app.SetWorld(sess.WorldStatus())

	// Warm the shared model catalog in the background so a slow or
	// unreachable provider endpoint never delays TUI launch; the control
	// channel is buffered and the control loop starts inside app.Run().
	catalog := newModelCatalog(cfg, nil)
	go warmModelList(catalog, func(snap modelSnapshot) { sendModelList(controlCh, snap) })

	app.OnModelSelect = func(model string) {
		parts := strings.SplitN(model, "/", 2)
		var providerName, modelName string
		if len(parts) == 2 {
			providerName = parts[0]
			modelName = parts[1]
		} else {
			providerName = parts[0]
		}
		sess.SetModel(providerName, modelName)
		app.SetProvider(providerName)
		app.SetModel(modelName)
	}
	// Persona palette (yaah-21uk.12): pick validates and SWITCHES the
	// current session — the persona body, budget and tool gate land on
	// the next run. The ephemeral line is the confirmation and the
	// info-pane mode badge re-derives from the persona.
	app.SetAgentPicker(personaNames(), personaDescriptions())
	app.OnAgentSelect = func(name string) {
		reply, _ := sess.switchPersona(name)
		app.SetMode(sess.Mode())
		// A routing persona re-points the session's provider/model
		// (parity W1): the header must show the model the next run uses.
		app.SetProvider(sess.ProviderName())
		app.SetModel(sess.ModelName())
		app.SetEphemeral(reply)
	}
	// Mode picker (W6): selection switches the CURRENT session's mode axis
	// (read-only/full), the interactive parity surface for ACP
	// session/set_mode. The word is already validated vocabulary; the reply
	// is the confirmation and the info-pane badge re-derives.
	app.OnModeSelect = func(word string) {
		reply, _ := sess.switchMode(word)
		app.SetMode(sess.Mode())
		app.SetEphemeral(reply)
	}

	var cancelAgent context.CancelFunc
	app.OnSubmit = func(input string) {
		ctx, cancel := context.WithCancel(context.Background())
		cancelAgent = cancel
		app.AddPrompt(input)
		app.ShowThinking()
		go sess.RunPrompt(ctx, input)
	}
	app.OnAbort = func() {
		app.HideThinking()
		if cancelAgent != nil {
			cancelAgent()
			cancelAgent = nil
		}
	}
	app.OnCompact = func() {
		go sess.Compact()
	}
	app.OnWorkflowRun = func(specPath string) {
		// A bare name resolves against the search dirs (default-location
		// feature) before the read guard; a path passes through unchanged.
		if resolved, err := resolveWorkflowSpec(specPath, nil); err != nil {
			sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
				Path:   specPath,
				OK:     false,
				Report: err.Error(),
			}, tuiSendTimeout)
			return
		} else {
			specPath = resolved
		}
		// Read-only guard before we spend anything.
		st, err := os.Stat(specPath)
		if err != nil {
			sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
				Path:   specPath,
				OK:     false,
				Report: fmt.Sprintf("cannot read spec: %v", err),
			}, tuiSendTimeout)
			return
		}
		if st.IsDir() {
			sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
				Path:   specPath,
				OK:     false,
				Report: fmt.Sprintf("cannot read spec: %s is a directory, not a file", specPath),
			}, tuiSendTimeout)
			return
		}
		// Single-flight: the runner exchanges its run-specific retained
		// runtime through a process-global pointer, so two overlapping runs
		// would cross wires on proposal-lane work. Refuse rather than race.
		if !workflowRunGate.TryLock() {
			sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
				Path:   specPath,
				OK:     false,
				Report: "a workflow run is already in progress",
			}, tuiSendTimeout)
			return
		}
		go func() { // do not block the UI; a run can take minutes
			defer workflowRunGate.Unlock()
			var buf bytes.Buffer
			c := &cobra.Command{Use: "workflow run"}
			c.SetOut(&buf) // capture the CLI's own report text
			c.SetErr(&buf)
			err := runWorkflowRun(c, specPath, workflowRunOptions{})
			report := buf.String()
			if err != nil && strings.TrimSpace(report) == "" {
				// The run failed before it could write a report; surface the
				// error rather than an empty body under a "failed" header.
				report = err.Error()
			}
			sendCtrlMsg(controlCh, nil, &control.WorkflowRunReport{
				Path:   specPath,
				OK:     err == nil,
				Report: report,
			}, tuiSendTimeout)
		}()
	}
	app.OnWorkflowList = func() {
		go func() {
			cwd, err := os.Getwd()
			if err != nil {
				sendCtrlMsg(controlCh, nil, &control.Status{Text: err.Error()}, tuiSendTimeout)
				return
			}
			specs, err := discover.List(cwd)
			if err != nil {
				sendCtrlMsg(controlCh, nil, &control.Status{Text: err.Error()}, tuiSendTimeout)
				return
			}
			entries := make([]control.WorkflowListEntry, 0, len(specs))
			for _, s := range specs {
				entries = append(entries, control.WorkflowListEntry{Name: s.Name, Path: s.Path})
			}
			sendCtrlMsg(controlCh, nil, &control.WorkflowList{Specs: entries}, tuiSendTimeout)
		}()
	}
	app.OnWorkflowPicker = func() {
		// Runs on the UI goroutine from HandleCommand; discovery is a local
		// read of two directories, so it is safe to do inline.
		cwd, err := os.Getwd()
		if err != nil {
			app.SetEphemeral(err.Error())
			return
		}
		specs, err := discover.List(cwd)
		if err != nil {
			app.SetEphemeral(err.Error())
			return
		}
		if len(specs) == 0 {
			app.SetEphemeral("no workflow specs found")
			return
		}
		entries := make([]workflowpicker.Entry, 0, len(specs))
		for _, s := range specs {
			entries = append(entries, workflowpicker.Entry{Name: s.Name, Path: s.Path})
		}
		app.ShowWorkflowPicker(entries, func(name string) {
			if app.OnWorkflowRun != nil {
				app.OnWorkflowRun(name)
			}
		})
	}
	app.OnClear = func() {}
	app.OnSteer = func(text string) {
		// Flush in-flight streamed text and echo the steer as a prompt so
		// what woke the turn is visible where the response lands, after
		// the assistant text that was streaming when it was sent.
		app.EchoPrompt(text)
		sess.Steer(text)
	}
	app.OnFollowUp = func(text string) {
		app.EchoPrompt(text)
		sess.FollowUp(text)
	}
	app.OnStop = func() {
		app.HideThinking()
		if cancelAgent != nil {
			cancelAgent()
			cancelAgent = nil
		}
	}

	app.ControlCh = controlCh
	sess.SetView(app)
	// The TUI accepts harness-initiated turns: a watch that fires while the
	// terminal is idle starts a turn through the session's run gate. The
	// policy lives here, in the driver, so headless surfaces can differ.
	if sess.IdleWakeTurnsEnabled() {
		sess.SetWakeTurnRequester(sess.StartWakeTurn)
	} else {
		app.SetEphemeral("idle wake turns disabled (agents.default.idle_wake_turns: false)")
	}

	// One shared bridge for questions and approvals (internal/control): the
	// TUI's only job is to be the transport.
	installControlBridges(sess, ctrlSurface{ch: controlCh})

	// Resume durable watches only now that the view, control channel and
	// wake policy are installed; a missed wake must render, not run unheard.
	sess.Start()

	// A workflow launcher (yaah tui workflow run <spec>) seeds the session's
	// first prompt instead of running the command fire-and-forget: the run
	// then executes inside the session, so progress streams and the report
	// lands in the conversation.
	if initialSpec != "" {
		app.SetInitialPrompt(workflowSeedPrompt(initialSpec))
	}

	// Suppress stderr while the TUI is on the alt screen. Anything written
	// to stderr (slow-refresh logs, MCP warnings, tool output) would bleed
	// through and corrupt the layout.
	origStderr := os.Stderr
	devNull, devNullErr := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if devNullErr == nil {
		os.Stderr = devNull
		log.SetOutput(devNull)
	}
	defer func() {
		// Land queued slow-render diagnostics before the alt screen is
		// torn down and stderr is restored.
		app.FlushSlowLog()
		os.Stderr = origStderr
		log.SetOutput(origStderr)
		if devNull != nil {
			devNull.Close()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(origStderr, "TUI panic: %v\n", r)
		}
	}()

	return app.Run()
}

// workflowSeedPrompt is the v1 prompt-seed for `yaah tui workflow run <spec>`:
// it asks the orchestrator to invoke the workflow_run tool on the given spec
// as its first action, then report the result. A later iteration (v2) will
// pre-seed the tool call deterministically rather than asking the model to
// make it, but the outcome is the same composition: the tool, not a mode
// switch, drives the run.
func workflowSeedPrompt(spec string) string {
	return fmt.Sprintf("Run the workflow at %q with the workflow_run tool, then report the result.", spec)
}

// modelListAttempts bounds how many discovery retries the warm loop runs. A
// provider that never answers is not worth a session-long probe: the next
// process start runs discovery again from scratch.
const modelListAttempts = 3

// warmModelList publishes the model catalog and keeps it honest.
//
// The first Load is bounded by the catalog's warm budget, so a provider that
// misses that window is absent from the snapshot. While the snapshot is
// unsettled, re-read it after the catalog's cooldown and deliver it again when it
// changed, so a provider slower than the budget does not stay absent for the life
// of the process.
//
// deliver is called once per snapshot worth showing. A caller pushing to a lane
// that can be replaced (the web's SSE stream) resolves the current lane inside
// deliver, so a retry lands wherever the client is by the time it fires.
func warmModelList(catalog *modelCatalog, deliver func(modelSnapshot)) {
	ctx := context.Background()
	snap := catalog.Load(ctx)
	deliver(snap)
	for attempt := 0; attempt < modelListAttempts && !snap.Settled; attempt++ {
		time.Sleep(catalog.retry)
		next := catalog.Current(ctx)
		if !slices.Equal(next.IDs, snap.IDs) {
			deliver(next)
		}
		// Adopt the result either way: a retry that finished discovery without
		// changing the ids is still terminal, and keeping the old snapshot would
		// sleep through the remaining attempts for nothing. A settled snapshot is
		// authoritative, so it may replace the shown list even when the provider
		// now offers fewer models.
		snap = next
	}
}

func sendModelList(ch chan<- control.Msg, snap modelSnapshot) {
	msg := &control.ModelList{Models: snap.IDs, ProviderNames: snap.ProviderNames}
	sendCtrlMsg(ch, nil, msg, tuiSendTimeout)
}
