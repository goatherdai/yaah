package tui

import (
	"strings"
	"unicode"

	"github.com/goatherdai/yaah/internal/tui/components/command"
)

// modePickerNames and modePickerDescriptions back the :mode picker. The
// words mirror the mode-axis vocabulary switchMode validates (read-only,
// full) — the same axis ACP session/set_mode maps onto — and the picker
// can only emit these, so selection hands the driver a validated word.
var modePickerNames = []string{"read-only", "full"}

var modePickerDescriptions = map[string]string{
	"read-only": "inspection set only — read-only, no modification",
	"full":      "full toolset",
}

// HandleCommand dispatches a colon command. It reports whether the command
// consumed the input — false when it only complained (missing argument, no turn
// to steer, unwired command), so the caller can keep the text for editing
// instead of silently deleting what the user typed.
func (t *App) HandleCommand(cmd command.Cmd, arg string) bool {
	switch cmd {
	case command.CmdQuit:
		t.Stop()
		return true
	case command.CmdClear:
		t.doClear()
		return true
	case command.CmdHelp:
		t.ShowHelp()
		return true
	case command.CmdCompact:
		if t.Hooks.OnCompact != nil {
			t.Hooks.OnCompact()
			return true
		}
		return false
	case command.CmdStop:
		// An explicit :stop is clear intent — no arming — but it must
		// not fabricate a stop echo when nothing runs.
		if !t.agentActive {
			t.SetEphemeral("no turn is running — nothing to stop")
			return false
		}
		if t.Hooks.OnStop == nil && t.Hooks.OnAbort == nil {
			t.SetEphemeral("stopping is unavailable in this session")
			return false
		}
		t.requestStop()
		return true
	case command.CmdSteer:
		if arg == "" {
			t.SetEphemeral("usage: " + command.Usage(command.CmdSteer))
			return false
		}
		// Steering targets a turn in flight. With nothing running the text would
		// sit in the steer channel until SteerMiddleware.PrepareStep drains it
		// into the FIRST step of the next turn — silently steering an unrelated,
		// later run. Refuse instead.
		if !t.agentActive {
			t.SetEphemeral("no turn is running — nothing to steer")
			return false
		}
		if t.Hooks.OnSteer == nil {
			t.SetEphemeral("steering is unavailable in this session")
			return false
		}
		t.Hooks.OnSteer(arg)
		return true
	case command.CmdFollowUp:
		if arg == "" {
			t.SetEphemeral("usage: " + command.Usage(command.CmdFollowUp))
			return false
		}
		// With no turn running there is nothing to follow up on, and the text
		// would otherwise leak into a later turn's follow-up drain. Sending it
		// as a prompt is what the user means.
		if !t.agentActive {
			if t.Hooks.OnSubmit == nil {
				t.SetEphemeral("no turn is running and there is nowhere to send this")
				return false
			}
			t.SetEphemeral("no turn is running — sent as a message")
			t.Hooks.OnSubmit(arg)
			return true
		}
		if t.Hooks.OnFollowUp == nil {
			t.SetEphemeral("follow-ups are unavailable in this session")
			return false
		}
		t.Hooks.OnFollowUp(arg)
		return true
	case command.CmdVerbose:
		t.verbose = !t.verbose
		t.refreshMessages()
		return true
	case command.CmdTop:
		// Jump to the top of the painted window; when older items are
		// collapsed behind the seam, load the next backfill batch first
		// so "top" always lands on readable content. Unlike the scroll
		// paths, the backfill is unconditional: ":top" is an explicit
		// jump and the viewport may be anywhere, so the seam-row gate
		// would leave it landing on the seam line itself.
		t.tr.SetScrolled(true)
		t.backfill()
		t.pane.ScrollToBeginning()
		return true
	case command.CmdBottom:
		// Back at the end: return to the live tail — the reset re-windows
		// to the trailing budget, and a reveal window is repainted since
		// its bottom seam hides the tail.
		t.returnToTail()
		t.pane.ScrollToEnd()
		return true
	case command.CmdBanner:
		t.toggleBanner()
		return true
	case command.CmdModel:
		t.ShowModelPicker(t.availableModels, t.providerNames, func(model string) {
			if t.Hooks.OnModelSelect != nil {
				t.Hooks.OnModelSelect(model)
			}
		})
		return true
	case command.CmdAgent:
		// The picker is the argument surface: a typed ":agent name" opens
		// the same list (the CmdModel precedent). Picking hands the name
		// to the driver, which sets the next-session override.
		if t.Hooks.OnAgentSelect == nil {
			t.SetEphemeral("agent selection is unavailable in this session")
			return false
		}
		t.ShowAgentPicker(t.agentNames, t.agentDescs, func(name string) {
			t.Hooks.OnAgentSelect(name)
		})
		return true
	case command.CmdMode:
		// Same picker-as-argument shape: the two mode words are the
		// picker's entries, and selection hands the word to the driver,
		// which switches this session's mode axis. The word the driver
		// receives is already validated vocabulary — the picker cannot
		// emit anything else — so the typed arg (if any) is ignored, like
		// :agent.
		if t.Hooks.OnModeSelect == nil {
			t.SetEphemeral("mode switching is unavailable in this session")
			return false
		}
		t.ShowAgentPicker(modePickerNames, modePickerDescriptions, func(word string) {
			t.Hooks.OnModeSelect(word)
		})
		return true
	case command.CmdSearch:
		if arg == "" {
			t.SetEphemeral("usage: " + command.Usage(command.CmdSearch))
			return false
		}
		t.searchMessages(arg)
		return true
	case command.CmdWorkflow:
		arg = strings.TrimSpace(arg)
		verb := arg
		rest := ""
		if i := strings.IndexFunc(arg, unicode.IsSpace); i >= 0 {
			verb = arg[:i]
			rest = strings.TrimSpace(arg[i:])
		}
		switch verb {
		case "":
			// Bare :workflow is useless without a verb (RequiresArg).
			t.SetEphemeral("usage: " + command.Usage(command.CmdWorkflow))
			return false
		case "list":
			if t.Hooks.OnWorkflowList == nil {
				t.SetEphemeral("workflow not available in this session")
				return false
			}
			if rest != "" {
				t.SetEphemeral("usage: :workflow list")
				return false
			}
			t.Hooks.OnWorkflowList()
			return true
		case "run":
			if t.Hooks.OnWorkflowRun == nil {
				t.SetEphemeral("workflow not available in this session")
				return false
			}
			if rest == "" {
				// No spec: open the selector, falling back to usage when the
				// picker is unavailable.
				if t.Hooks.OnWorkflowPicker == nil {
					t.SetEphemeral("usage: :workflow run <spec.yaml|spec.json>")
					return false
				}
				t.Hooks.OnWorkflowPicker()
				return true
			}
			t.Hooks.OnWorkflowRun(rest)
			return true
		default:
			t.SetEphemeral("usage: " + command.Usage(command.CmdWorkflow))
			return false
		}
	default:
		// Reachable for commands that are declared and parseable but have no
		// handler yet (roles, login, logout, session, mcp). Say so rather than
		// accepting the input and doing nothing.
		if name := command.Name(cmd); name != "" {
			t.SetEphemeral(":" + name + " is not available in this session")
		}
		return false
	}
}

// labelFor returns the palette label for cmd, or "" if cmd has no entry.
func labelFor(entries []command.Entry, cmd command.Cmd) string {
	for _, e := range entries {
		if e.Cmd == cmd {
			return e.Label
		}
	}
	return ""
}

func (t *App) showCommandList() {
	t.hideSuggestions()
	entries := append([]command.Entry{}, command.DefaultEntries()...)
	command.ShowList(t.App, t.Pages, entries, func(cmd command.Cmd) {
		t.App.SetFocus(t.Input)
		t.focus = focusNormal
		if command.RequiresArg(cmd) {
			// The palette cannot collect an argument, so hand the user the
			// syntax in the input box rather than dispatching a silent no-op.
			t.Input.SetText(":"+labelFor(entries, cmd)+" ", true)
			t.SetEphemeral("usage: " + command.Usage(cmd))
			return
		}
		t.HandleCommand(cmd, "") // palette has no argument to consume
	}, func() {
		t.App.SetFocus(t.Input)
		t.focus = focusNormal
	})
	t.focus = focusCommandPalette
}

func (t *App) toggleCommandPalette() {
	if t.Pages.HasPage(command.PageName) {
		t.Pages.RemovePage(command.PageName)
		t.App.SetFocus(t.Input)
		t.focus = focusNormal
	} else {
		t.showCommandList()
	}
}
