package yaah

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/acp"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/cwdpath"
	"github.com/goatherdai/yaah/internal/mcp"
	"github.com/goatherdai/yaah/internal/memory"
	"github.com/goatherdai/yaah/internal/permissions"
	"github.com/goatherdai/yaah/internal/retention"
	"github.com/goatherdai/yaah/internal/sessions"
	"github.com/goatherdai/yaah/internal/skills"
	"github.com/goatherdai/yaah/internal/workflow/discover"
	"github.com/spf13/cobra"
)

// === ACP serve command ===

// acpServeCmd is the original top-level spelling, kept working because editor
// ACP configs reference it by name (see docs/acp.md). The canonical spelling
// is 'yaah acp serve'.
var acpServeCmd = newACPServeCmd("acp-serve", true)

// acpCmd groups the ACP surface. Today it holds only 'serve', but naming it
// after the protocol keeps the spelling consistent with 'yaah mcp serve'
// instead of putting a verb inside the server name.
var acpCmd = &cobra.Command{
	Use:   "acp",
	Short: "Agent Client Protocol (ACP) integration",
}

func newACPServeCmd(use string, deprecated bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Expose yaah as an Agent Client Protocol v1 (ACP) agent over stdio",
		Long: `Serve yaah as an Agent Client Protocol (ACP) v1 agent,
speaking the protocol used by editors such as Zed and VS Code
(https://agentclientprotocol.com).

The protocol uses newline-delimited JSON-RPC 2.0 on stdin/stdout. All
diagnostics go to stderr so stdout stays clean for protocol JSON.

Implemented:
  initialize                  Handshake (protocol version 1)
  authenticate                Validate a declared auth method (method id only;
                              no credential presence check, no network call)
  logout                      Remove stored OAuth credentials (advertised via
                              agentCapabilities.auth.logout)
  session/new                 Create a session (honors the client's cwd)
  session/load                Resume a session from ~/.yaah state.db in a
                              fresh process; replays the conversation as
                              session/update notifications before answering
  session/resume              Re-attach to a persisted session without
                              replaying history
  session/list                Page through persisted sessions (cwd filter)
  session/close               Cancel in-flight work and free a session
  session/delete              Remove a session from the store
  session/prompt              Run a turn; responds with a stopReason
  session/cancel              Cancel the in-flight turn (notification)
  session/set_mode            Switch Plan/Build mode (backed by personas);
                              plan withholds mutating
                              tools and emits config_option_update
  session/set_agent           Switch the session's persona; validates against
                              the persona registry and answers the effective
                              persona
  session/update              Streaming updates: message/thought chunks,
                              tool calls, plans, usage, compaction
  session/request_permission  Tool approvals delegated to the client
  session/set_config_option   Change a config option (mode/model picker) mid-session
  notifications/initialized   Client readiness announcement (accepted; no-op)

Several conversations may coexist in one process; the persisted session
store (~/.yaah state.db) is shared, so list/resume/close/delete operate
on it the same way the yaah CLI does. A session/load or session/resume
naming an id the store never issued is rejected.

Authentication is by provider credential configured yaah-side
(~/.yaah/config.yaml, or an OAuth token obtained via the web UI login
action):
initialize advertises the accepted method ids as agent-typed authMethods,
and authenticate validates the chosen id without checking that a key is
present. The approval bridge, tool-kind classification, and sandbox
layers are yaah-side policy and are not surfaced through ACP. See
docs/acp.md for the full surface, persisted state, and client setup
details.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE:          runACPServe,
	}
	if deprecated {
		cmd.Deprecated = "use 'yaah acp serve' instead"
	}
	return cmd
}

func init() {
	rootCmd.AddCommand(acpServeCmd)
	rootCmd.AddCommand(acpCmd)
	acpCmd.AddCommand(newACPServeCmd("serve", false))
}

// newACPAgentSession builds one agent session for the ACP server. An
// empty ResumeID starts a fresh conversation; otherwise the persisted
// session of that id is restored from the SQLite session store
// (~/.yaah state.db via internal/memory) — that restore is how
// session/load rejects foreign ids and supplies the replay history.
// The ACP client owns the session lifecycle, so --resume from the
// command line is ignored: each session is built from the request that
// created or loaded it. The bare picker form has nothing interactive to
// pick with and is rejected rather than silently ignored.
func newACPAgentSession(build acp.BuildOptions) (acp.Session, error) {
	if resumeSessionID == resumePickerSentinel {
		return nil, fmt.Errorf("--resume without a value opens the interactive picker, which is not available in ACP mode; omit the flag — sessions are named by the client's session/new and session/load requests")
	}
	opts := sessionOptionsFromFlags()
	opts.ResumeSessionID = build.ResumeID
	opts.Cwd = build.Cwd
	opts.MCPManifests = mcpManifestsFromACP(build.MCPServers)
	sess, err := newAgentSessionWithOptions(opts, false, false)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	// ACP has no client-initiated prompt for a harness wake, so no idle
	// requester is installed: rehydration arms durable watches and any fire
	// queues as a follow-up for the next session/prompt.
	sess.Start()
	return sess, nil
}

// mcpManifestsFromACP converts ACP's stdio server entries to the
// manifest form the MCP client starts.
func mcpManifestsFromACP(refs []acp.McpServerRef) map[string]mcp.Manifest {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]mcp.Manifest, len(refs))
	for _, ref := range refs {
		env := make(map[string]string, len(ref.Env))
		for _, e := range ref.Env {
			if e.Name != "" {
				env[e.Name] = e.Value
			}
		}
		out[ref.Name] = mcp.Manifest{
			Command:   ref.Command,
			Args:      ref.Args,
			Env:       env,
			Transport: "stdio",
		}
	}
	return out
}

// acpListSessions adapts the engine-level session registry to the
// session/list callback: registry rows become ACP SessionInfo entries, whose
// updatedAt is the ISO 8601 timestamp of the last persisted activity and whose
// _meta carries the picker's message count. Exactly one registry page is
// served — the callback contract is one page plus its cursor — so this uses
// Page, not List. A malformed cursor is re-flagged as the ACP sentinel so the
// surface errors with invalid params instead of an internal error.
//
// The cwdFilter arrives already canonical (internal/acp canonicalizes it on
// intake); the registry canonicalizes again, which is idempotent and safe.
func acpListSessions(reg *sessions.Registry) func(cwdFilter, cursor string) ([]acp.SessionInfo, string, error) {
	return func(cwdFilter, cursor string) ([]acp.SessionInfo, string, error) {
		rows, next, err := reg.Page(sessions.Filter{CWD: cwdFilter, Cursor: cursor})
		if err != nil {
			if errors.Is(err, sessions.ErrInvalidCursor) {
				return nil, "", fmt.Errorf("%w: %v", acp.ErrInvalidCursor, err)
			}
			return nil, "", err
		}
		out := make([]acp.SessionInfo, 0, len(rows))
		for _, row := range rows {
			out = append(out, acp.SessionInfo{
				SessionID: row.ID,
				Cwd:       row.CWD,
				Title:     row.Title,
				// Store timestamps are Unix seconds (time.Unix origin).
				UpdatedAt: time.Unix(row.UpdatedAt, 0).UTC().Format(time.RFC3339),
				Meta:      sessionMeta(row.MessageCount),
			})
		}
		return out, next, nil
	}
}

// sessionMeta builds the ACP `_meta` payload for one session/list entry.
// A session with no persisted messages reports no messageCount at all
// rather than a zero, so a client can tell "empty" from "unknown".
func sessionMeta(messageCount int) *acp.SessionMeta {
	if messageCount <= 0 {
		return nil
	}
	return &acp.SessionMeta{MessageCount: messageCount}
}

// acpDeleteSession adapts the session registry to the session/delete
// callback. An unknown id maps to the ACP sentinel: deleting a
// deleted-or-never-created session succeeds silently per spec.
func acpDeleteSession(reg *sessions.Registry) func(id string) error {
	return func(id string) error {
		if err := reg.Delete(id); err != nil {
			if errors.Is(err, sessions.ErrNotFound) {
				return fmt.Errorf("%w: %s", acp.ErrNoSession, id)
			}
			return err
		}
		return nil
	}
}

// acpLogout removes stored OAuth credentials for every configured OAuth
// provider. ACP logout has no provider argument, so "log out" means all
// yaah-managed OAuth tokens.
func acpLogout() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	var errs []error
	for _, name := range oauthProviderNames(cfg) {
		if err := logoutOAuth(cfg, name, func(string) {}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// acpAvailableCommands exposes discovered skills as ACP slash commands. A
// selected command is sent back as ordinary prompt text; the server translates
// it into a skill-tool load instruction (see translateCommandPrompt).
func acpAvailableCommands(cwd string) []acp.AvailableCommand {
	found := skills.Discover(skillSearchPathsFor(cwd))
	out := make([]acp.AvailableCommand, 0, len(found))
	for _, skill := range found {
		out = append(out, acp.AvailableCommand{
			Name:        skill.Name,
			Description: skill.Description,
			Input:       &acp.AvailableCommandInput{Hint: "arguments for " + skill.Name},
		})
	}
	return out
}

// acpListWorkflows adapts the discover package to the workflow/list
// callback: the discovered specs, in search order, for a client's picker.
func acpListWorkflows() func(cwd string) ([]acp.WorkflowInfo, error) {
	return func(cwd string) ([]acp.WorkflowInfo, error) {
		specs, err := discover.List(cwd)
		if err != nil {
			return nil, err
		}
		out := make([]acp.WorkflowInfo, 0, len(specs))
		for _, sp := range specs {
			out = append(out, acp.WorkflowInfo{Name: sp.Name, Path: sp.Path})
		}
		return out, nil
	}
}

// acpRunWorkflow adapts workflow_run's execution path to the workflow/run
// callback. It resolves the spec against the search dirs, executes it, and
// captures the rendered report. The run is synchronous here; the ACP server
// invokes the callback off the dispatch loop (see handleWorkflowRun).
func acpRunWorkflow() func(cwd, spec string) (acp.WorkflowRunResult, error) {
	return func(cwd, spec string) (acp.WorkflowRunResult, error) {
		resolved, err := discover.Resolve(cwd, spec)
		if err != nil {
			return acp.WorkflowRunResult{}, err
		}
		// Same single-flight gate as the TUI and tool paths: the runner
		// exchanges run-specific retained runtime through a process-global
		// pointer, so overlapping runs would cross wires.
		if !workflowRunGate.TryLock() {
			return acp.WorkflowRunResult{OK: false, Report: "a workflow run is already in progress"}, nil
		}
		defer workflowRunGate.Unlock()
		var out, errOut bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		err = runWorkflowRun(cmd, resolved, workflowRunOptions{})
		report := out.String()
		if errOut.Len() > 0 {
			report += "\n" + errOut.String()
		}
		if err != nil {
			if strings.TrimSpace(report) == "" {
				// The run failed before it could write a report; surface the
				// error rather than handing the client an empty body.
				report = err.Error()
			}
			return acp.WorkflowRunResult{OK: false, Report: report}, nil
		}
		return acp.WorkflowRunResult{OK: true, Report: report}, nil
	}
}

func runACPServe(cmd *cobra.Command, args []string) error {
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, Dim("yaah acp-serve:")+" "+format+"\n", args...)
	}
	fmt.Fprintf(os.Stderr, "%s starting ACP server (stdio)...\n", Dim("yaah acp-serve:"))

	// The engine-level session registry is the lifecycle seam: the
	// session/list and session/delete callbacks below share it with the
	// yaah sessions CLI, the web session picker, and the interactive
	// resume picker. Per-conversation agent sessions open their own store
	// handles inside the factory, so this one is used read-and-delete only.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	reg, err := sessions.OpenDefault()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer reg.Close()
	// Kernel-first deletion: session/delete evicts the session's trace
	// paths before the state cascade (docs/retention-gc.md §5).
	reg.WithEvictor(retention.NewEvictor(cfg.Agent.Default.TraceStoreConfig()))

	srv := acp.NewServer(
		newACPAgentSession,
		version,
		logf,
		newACPModelCatalog(logf),
	)
	srv.ListSessions = acpListSessions(reg)
	srv.DeleteSession = acpDeleteSession(reg)
	srv.AvailableCommands = acpAvailableCommands
	srv.Logout = acpLogout
	srv.ListWorkflows = acpListWorkflows()
	srv.RunWorkflow = acpRunWorkflow()

	// Saved approvals (yaah-x6vi.6): allow-always answers persist
	// project-scoped so a later session in the workspace does not re-ask.
	// The store refines the belt, it never gates it — an open failure
	// degrades to session-only memory and the server logs every one.
	if db, err := memory.OpenDefault(); err == nil {
		defer db.Close()
		srv.SaveApproval = func(cwd, tool, resource string) error {
			// The durable carve-out (yaah-x6vi.7): a persisted "always" for
			// a credential-shaped target is refused at the save seam.
			//
			// This is defence in depth, not the live path: approval.go
			// decides the carve-out on the call before ever reaching here,
			// logs it, and leaves the ask asking. The seam exists so a
			// future writer of SaveApproval cannot reintroduce a persisted
			// always for one of these targets, so its error names the
			// resource the caller offered.
			if permissions.SensitiveResource(tool, resource) {
				return fmt.Errorf("not saving always-allow for sensitive resource %q: it stays ask (grant it explicitly with a permissions rule instead)", resource)
			}
			return db.SaveSavedPermission(memory.SavedPermission{
				ProjectID: cwdpath.CanonicalStored(cwd),
				Action:    tool,
				Resource:  resource,
			})
		}
	} else {
		logf("warning: saved approvals unavailable (store open failed): %v", err)
	}
	return srv.Run(cmd.Context())
}
