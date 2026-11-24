package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// portIsNil reports whether an interface-valued dependency is absent,
// including a typed-nil pointer stored in it. Wiring may assign a nil
// *supervision.Service to the Svc interface when tracing is on but advisory
// rules are off; a plain == nil check would miss it and status would panic.
func portIsNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// runBridge is the supervisor tool's consumer-side view of the session's
// supervision registry. The concrete *supervision.Registry satisfies it
// structurally — its Redirect returns a supervisionapi.RedirectHandle,
// the type this interface names — and the supervision package the
// wiring injects never imports this tools package back.
type runBridge interface {
	Inject(scopeID, guidance string) error
	Cancel(scopeID string) (<-chan struct{}, error)
	ScopeIDs() []string
	// Generation reports the scope's live generation — the token a
	// redirect claims against (yaah-8gxg.1's compare-and-swap).
	Generation(scopeID string) (uint64, bool)
	// Redirect CAS-claims the scope's current generation for a handoff:
	// single winner per generation (ErrAlreadyClaimed), ErrNoRun when
	// the generation no longer names a live run.
	Redirect(scopeID string, fromGen uint64) (supervisionapi.RedirectHandle, error)
	// DepositRedirect attaches the handoff payload (the successor's
	// guidance plus the live-revert flag) to a claim made by Redirect.
	DepositRedirect(scopeID string, fromGen uint64, req supervisionapi.RedirectRequest) error
}

// firingSource is the status view's consumer-side view of the advisory
// rules service.
type firingSource interface {
	Firings() []supervisionapi.Firing
}

// haltQuiesceTimeout bounds the wait for a halted run to stop before
// the kernel-side sandbox teardown proceeds anyway — the same
// cancel-then-join-bounded shape as the jobs manager's CloseAndWait.
const haltQuiesceTimeout = 10 * time.Second

// SupervisorTool lets the orchestrator supervise sub-agent execution:
// intervention (inject/halt/redirect) on running sub-agents, and scope inspection
// (list_scopes, status). Judgement of a sub-agent's work is not an action
// here — it is the orchestrator settling the proposal.
//
// The tool operates on the session's scope manager and trace store,
// injected by the wiring (plan 07, H4) — the same instances the
// goatherd_trace middleware and the sub-agent runner carry, so the
// supervisor, the runner, and the tracing share one registry.
type SupervisorTool struct {
	TraceDir string

	// RootScopeID is the session's root scope (adoption plan D1), set by
	// the wiring. Inject into it steers the session itself (its steering
	// channel, plan A2). Halt maps to cancelling the session's current
	// run — the ctrl-C equivalent — and carries the same consequences:
	// the root scope goes non-active, so later sub-agent scopes fall
	// back to flat creation and supervision degrades to scope-less
	// tracing for the rest of the process.
	RootScopeID string

	// ScopeManager is the session's scope registry (plan 07, H4):
	// session-owned and injected by the wiring, like the run registry.
	// Nil when tracing is disabled.
	ScopeManager *goatherd.ScopeManager

	// HaltRoot is the agents.default.supervisor_halt_root opt-in
	// (adoption plan A3): root halt needs it, and it defaults off — a
	// model or a rule halting the interactive session is a big hammer.
	HaltRoot bool

	// Runs is the session's supervision run registry (adoption plan A2),
	// injected by the wiring — session-owned, not a process global. Nil
	// when tracing is disabled: inject then degrades honestly to the
	// trace-only outcome.
	Runs runBridge

	// Svc is the session's advisory-rules service (adoption plan B4):
	// status lists its rule firings — which rules acted, on which
	// scopes, how often. Nil when the supervisor stanza is not live;
	// status then reports no firings rather than guessing.
	Svc firingSource

	// Snapshot is the session's effect-stream snapshot source
	// (yaah-1050.1.1): a read-only join of the run registry with the
	// per-agent tool facts the trace already holds. Nil when tracing is
	// off — `snapshot` then says so instead of inventing agents.
	Snapshot supervisionapi.SnapshotSource
}

func (*SupervisorTool) Name() string { return "supervisor" }
func (*SupervisorTool) Description() string {
	return prompts.ToolDescription("supervisor")
}

func (*SupervisorTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["list_scopes", "inject", "halt", "redirect", "revert", "status", "snapshot"],
				"description": "list_scopes: show all scopes and their state; inject: push guidance into a scope (trace-only if no active run); halt: cancel a scope and bounded-quiesce it (root halt needs supervisor_halt_root); redirect: claim the scope's current run, deposit guidance for its successor, cancel it, and return immediately — the dispatch adopts a fresh loop on the same scope with the guidance as its first message; revert: redirect plus a workspace rollback — before the fresh loop the dispatch restores the sub-agent's workspace to its pre-run snapshot (a scope with no rollback substrate skips the rollback and logs a note); status: show supervision system status; snapshot: read a compact per-agent snapshot of what each sub-agent is doing (trailing tool calls, modified files, error counts, age)"
			},
			"scope_id": {
				"type": "string",
				"description": "Scope ID (required for inject, halt, redirect and revert)"
			},
			"guidance": {
				"type": "string",
				"description": "Guidance text to inject (required for inject; the successor's first message for redirect and revert)"
			}
		},
		"required": ["action"]
	}`)
}

// supervisorActionArgs is the parsed argument set for the supervisor tool:
// scope fields apply to inject/halt.
type supervisorActionArgs struct {
	Action   string `json:"action"`
	ScopeID  string `json:"scope_id"`
	Guidance string `json:"guidance"`
}

func (t *SupervisorTool) Execute(ctx context.Context, args string) (string, error) {
	var raw supervisorActionArgs
	if err := StrictToolArgs("supervisor", t.Schema(), args, &raw); err != nil {
		return "", err
	}

	mgr := t.ScopeManager
	if mgr == nil {
		return "", fmt.Errorf("supervisor: goatherd scope manager unavailable (trace store failed to open)")
	}
	// Normalize typed-nil ports once, so inject/halt/status all see a true
	// nil when wiring supplied a nil concrete pointer.
	runs, svc := t.Runs, t.Svc
	if portIsNil(runs) {
		runs = nil
	}
	if portIsNil(svc) {
		svc = nil
	}
	switch raw.Action {
	case "list_scopes":
		return executeListScopes(mgr)
	case "inject":
		if raw.ScopeID == "" || raw.Guidance == "" {
			return "", fmt.Errorf("supervisor: scope_id and guidance required for inject")
		}
		return executeInject(mgr, runs, raw.ScopeID, raw.Guidance)
	case "halt":
		if raw.ScopeID == "" {
			return "", fmt.Errorf("supervisor: scope_id required for halt")
		}
		if raw.ScopeID == t.RootScopeID && !t.HaltRoot {
			return "", fmt.Errorf("supervisor: halting the session root requires the supervisor_halt_root opt-in (agents.default.supervisor_halt_root); it stops the session's current run and ends scope-tree supervision for this process. Halt a sub-agent scope instead (see list_scopes)")
		}
		return executeHalt(mgr, runs, raw.ScopeID)
	case "redirect":
		if raw.ScopeID == "" || raw.Guidance == "" {
			return "", fmt.Errorf("supervisor: scope_id and guidance required for redirect")
		}
		if raw.ScopeID == t.RootScopeID && !t.HaltRoot {
			return "", fmt.Errorf("supervisor: redirecting the session root requires the supervisor_halt_root opt-in (agents.default.supervisor_halt_root); it stops the session's current run and no dispatch loop exists to adopt a replacement. Redirect a sub-agent scope instead (see list_scopes)")
		}
		return executeRedirect(mgr, runs, raw.ScopeID, raw.Guidance, false)
	case "revert":
		if raw.ScopeID == "" || raw.Guidance == "" {
			return "", fmt.Errorf("supervisor: scope_id and guidance required for revert")
		}
		if raw.ScopeID == t.RootScopeID && !t.HaltRoot {
			return "", fmt.Errorf("supervisor: reverting the session root requires the supervisor_halt_root opt-in (agents.default.supervisor_halt_root); it stops the session's current run and no dispatch loop exists to adopt a replacement. Revert a sub-agent scope instead (see list_scopes)")
		}
		return executeRedirect(mgr, runs, raw.ScopeID, raw.Guidance, true)
	case "status":
		return executeStatus(mgr, runs, svc)
	case "snapshot":
		return executeSnapshot(t.Snapshot)
	default:
		return "", fmt.Errorf("supervisor: unknown action %q (valid: list_scopes, inject, halt, redirect, revert, status, snapshot)", raw.Action)
	}
}

func executeListScopes(mgr *goatherd.ScopeManager) (string, error) {
	all := mgr.AllScopes()
	if len(all) == 0 {
		return "No scopes registered.", nil
	}

	var sb string
	sb = "Scopes:\n"
	for _, s := range all {
		parentID := "(root)"
		if s.Parent() != nil {
			parentID = s.Parent().ID()
		}
		sb += fmt.Sprintf("  %s  owner=%s  state=%s  parent=%s\n",
			s.ID(), s.OwnerID(), s.State(), parentID)
	}
	return sb, nil
}

func executeInject(mgr *goatherd.ScopeManager, runs runBridge, scopeID, guidance string) (string, error) {
	scope, ok := mgr.Get(scopeID)
	if !ok {
		return "", fmt.Errorf("supervisor: scope %s not found", scopeID)
	}
	if err := scope.Inject(guidance); err != nil {
		return "", fmt.Errorf("supervisor: inject failed: %w", err)
	}
	// Supervision bridge (adoption plan A2): the trace fact above is the
	// record; the registry delivery is the intervention. Every outcome
	// below reports exactly what happened — the fact that the trace
	// already holds the intervention never turns a delivery failure
	// into a hard error the orchestrator would misread as "nothing was
	// recorded".
	if runs == nil {
		return fmt.Sprintf("Guidance recorded in the trace for %s; the supervision bridge is not active in this session.", scopeID), nil
	}
	switch err := runs.Inject(scopeID, guidance); {
	case err == nil:
		return fmt.Sprintf("Injected guidance into %s; delivered to the running loop.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrNoRun):
		return fmt.Sprintf("Guidance recorded in the trace for %s; no active run to deliver it to (the scope is not currently executing).", scopeID), nil
	case errors.Is(err, supervisionapi.ErrSteerBusy):
		return fmt.Sprintf("Guidance recorded in the trace for %s, but the run's steering buffer is full — it is not consuming guidance; consider halting the scope instead.", scopeID), nil
	default:
		return "", fmt.Errorf("supervisor: inject delivery failed: %w", err)
	}
}

func executeHalt(mgr *goatherd.ScopeManager, runs runBridge, scopeID string) (string, error) {
	scope, ok := mgr.Get(scopeID)
	if !ok {
		return "", fmt.Errorf("supervisor: scope %s not found", scopeID)
	}
	if scope.State() == goatherd.ScopeMerged {
		// Merged is terminal: the kernel refuses a halt beside the merge
		// record. Report the ending before touching any run — there is
		// nothing to cancel and nothing to quiesce, and the refusal must
		// not cost a haltQuiesceTimeout wait.
		return fmt.Sprintf("%s is already merged (terminal); nothing to halt — its effects are integrated and no run is using it.", scopeID), nil
	}
	// Halt bridge (adoption plan A3): cancel the run FIRST, then WAIT —
	// bounded — for it to quiesce before recording the kernel halt,
	// which tears down an owned sandbox (an isolated child's worktree).
	// Cancelling alone is not quiescence: an in-flight tool can still be
	// inside the very sandbox scope.Halt destroys next, the teardown
	// hazard the jobs manager's CloseAndWait documents. Every outcome
	// reports what happened; a finished run's halt stays a trace-side
	// record, never a phantom cancellation.
	var cancelled, quiesced, waited bool
	if runs != nil {
		quiesce, err := runs.Cancel(scopeID)
		switch {
		case err == nil:
			cancelled = true
			if quiesce != nil {
				waited = true
				select {
				case <-quiesce:
					quiesced = true
				case <-time.After(haltQuiesceTimeout):
					// Bounded proceed: the kernel halt still lands —
					// halt-while-running is its semantics — and the
					// report names the unquiesced run.
				}
			}
		case errors.Is(err, supervisionapi.ErrNoRun), errors.Is(err, supervisionapi.ErrNotHaltable):
			// fall through to the kernel-side halt below
		default:
			return "", fmt.Errorf("supervisor: halt delivery failed: %w", err)
		}
	}
	if err := scope.Halt(); err != nil {
		return "", fmt.Errorf("supervisor: halt failed: %w", err)
	}
	switch {
	case cancelled && (!waited || quiesced):
		return fmt.Sprintf("Halted %s; the running loop was cancelled and has stopped.", scopeID), nil
	case cancelled:
		return fmt.Sprintf("Halted %s; the run was cancelled but had not stopped within %s — an in-flight tool may have been interrupted by the sandbox teardown.", scopeID, haltQuiesceTimeout), nil
	default:
		return fmt.Sprintf("Halted %s in the trace; no active run to cancel (the scope is not currently executing).", scopeID), nil
	}
}

// executeRedirect claims the scope's live run for a handoff, deposits
// the successor's payload (guidance, plus revert's workspace-rollback
// mark — yaah-1050.1.3), cancels the current run so it returns, and
// reports immediately (yaah-8gxg.4). revert selects between the two
// handoffs: false is the plain redirect, true is the live revert — the
// same LLM-side effect either way, with the rollback decided by the
// dispatch's adoption loop from the deposited flag. It never waits for
// the replacement: the claim is a compare-and-swap the dispatch's
// parent loop adopts only when the cancelled run returns, and holding
// this call through that return is exactly the through-the-replacement
// blocking the redesign forbids. Every refusal — no live run, an
// already claimed generation, a deposit with no claim behind it — is a
// result rather than an error, so the orchestrator learns what happened
// and can re-inspect instead of retrying blind.
func executeRedirect(mgr *goatherd.ScopeManager, runs runBridge, scopeID, guidance string, revert bool) (string, error) {
	if _, ok := mgr.Get(scopeID); !ok {
		return "", fmt.Errorf("supervisor: scope %s not found", scopeID)
	}
	if runs == nil {
		return "", fmt.Errorf("supervisor: redirect needs the supervision bridge (not active in this session)")
	}
	gen, ok := runs.Generation(scopeID)
	if !ok {
		return fmt.Sprintf("No active run on %s to redirect (the scope is not currently executing); nothing was claimed.", scopeID), nil
	}
	handle, err := runs.Redirect(scopeID, gen)
	switch {
	case errors.Is(err, supervisionapi.ErrNoRun):
		// The generation moved between inspection and claim (the run
		// finished or a successor registered): re-inspect, don't retry.
		return fmt.Sprintf("The run on %s finished or was superseded while redirecting; nothing was claimed — inspect with list_scopes and retry if a run is live.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrAlreadyClaimed):
		return fmt.Sprintf("A redirect is already claimed for %s; the handoff is in progress, so only one replacement will start for this run.", scopeID), nil
	case err != nil:
		return "", fmt.Errorf("supervisor: redirect claim failed: %w", err)
	}
	if err := runs.DepositRedirect(scopeID, handle.Gen, supervisionapi.RedirectRequest{Guidance: guidance, Revert: revert}); err != nil {
		// The claim died between the two calls — the run ended or was
		// superseded, so the guidance has nowhere to land.
		return fmt.Sprintf("The redirect guidance for %s could not be recorded (%v); no replacement will start.", scopeID, err), nil
	}
	// Cancel the CURRENT run so its runOnce returns — then return. The
	// quiesce channel deliberately goes unread: waiting on it here would
	// hold the supervisor through the handoff (and the sequence's Done
	// only closes after the FINAL run), which is the blocking this
	// action exists to avoid.
	switch _, err := runs.Cancel(scopeID); {
	case err == nil:
		if revert {
			return fmt.Sprintf("Revert requested for %s: the current run was cancelled; the dispatch will roll the workspace back to the pre-run snapshot and start a fresh loop on the same scope. This call returns now — the replacement is adopted when that run returns.", scopeID), nil
		}
		return fmt.Sprintf("Redirect requested for %s: the current run was cancelled and its guidance will start a fresh loop on the same scope. This call returns now — the replacement is adopted when that run returns.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrNotHaltable):
		return fmt.Sprintf("Redirect claimed for %s, but the run registered no cancellation; the successor loop will start when the current run returns on its own.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrNoRun):
		return fmt.Sprintf("The run on %s ended as the redirect was requested; no replacement will start.", scopeID), nil
	default:
		return "", fmt.Errorf("supervisor: redirect cancel failed: %w", err)
	}
}

func executeStatus(mgr *goatherd.ScopeManager, runs runBridge, svc firingSource) (string, error) {
	active := mgr.ActiveScopes()
	all := mgr.AllScopes()
	out := fmt.Sprintf("Supervision status:\n  Scopes: %d active, %d total\n", len(active), len(all))
	if runs != nil {
		ids := runs.ScopeIDs()
		bridged := make(map[string]bool, len(ids))
		for _, id := range ids {
			bridged[id] = true
		}
		var unbridged []string
		for _, sc := range active {
			if id := sc.ID(); !bridged[id] {
				unbridged = append(unbridged, id)
			}
		}
		if len(unbridged) > 0 {
			out += fmt.Sprintf("  Active unbridged scopes: %d (no bridged run — leak candidates; the reaper discards them after its grace period)\n", len(unbridged))
			for i, id := range unbridged {
				if i == 5 {
					out += fmt.Sprintf("    … and %d more\n", len(unbridged)-5)
					break
				}
				out += fmt.Sprintf("    %s\n", id)
			}
		}
		out += fmt.Sprintf("  Bridged runs: %d\n", len(ids))
		for _, id := range ids {
			out += fmt.Sprintf("    %s (injectable)\n", id)
		}
	} else {
		out += "  Bridged runs: 0 (supervision bridge not active)\n"
	}
	switch {
	case svc == nil:
		out += "  Rule firings: 0 (supervision service not active)\n"
	default:
		firings := svc.Firings()
		if len(firings) == 0 {
			out += "  Rule firings: 0\n"
			break
		}
		out += "  Rule firings:\n"
		for _, f := range firings {
			out += fmt.Sprintf("    %s  %s  %dx (%s)", f.Rule, f.Scope, f.Count, f.Kind)
			// A blocking deny names what it most recently refused and
			// why; an advisory firing steers, and carries neither. The
			// detail is the latest refusal for this line's count and is
			// already bounded upstream, so one line stays one line no
			// matter how many — or how long — the refusals were.
			cmd := f.Command
			if cmd == "" && f.Kind == "deny" {
				// A shell call that carried no command at all: said
				// plainly rather than left blank.
				cmd = "(no command)"
			}
			if cmd != "" {
				out += fmt.Sprintf("  last_cmd=%q", cmd)
			}
			if f.Reason != "" {
				out += fmt.Sprintf("  reason=%q", f.Reason)
			}
			out += "\n"
		}
	}
	return out, nil
}

// snapshotRenderBudget bounds the rendered snapshot, well under
// substrate.ToolResultMaxBytes. The source already caps agents and trail
// lengths; this is the second cap, because a poll must never crowd out the
// rest of the turn whatever the source hands back.
const snapshotRenderBudget = 6000

// executeSnapshot renders the batched per-agent snapshot (yaah-1050.1.1):
// who is running, what each agent has called lately, what it has written,
// and how it has fared. It reads; it never reaches the worker, so a poll
// cannot perturb the context it summarizes.
func executeSnapshot(src supervisionapi.SnapshotSource) (string, error) {
	if portIsNil(src) {
		return "Agent snapshot unavailable: the effect-stream source is not wired in this session (tracing is off), so there are no per-agent facts to read.", nil
	}
	snap := src.Snapshot()
	head := "Agent snapshot at " + snap.At.UTC().Format(time.RFC3339) + fmt.Sprintf(": %d agent(s)", len(snap.Agents))
	if len(snap.Agents) == 0 {
		head += " — none registered; no dispatch is running or recently finished."
	} else {
		head += "\n"
	}
	out := head
	rendered, skipped := 0, 0
	for _, a := range snap.Agents {
		block := snapshotBlock(a, snap.At)
		if len(out)+len(block) > snapshotRenderBudget {
			skipped++
			continue
		}
		out += block
		rendered++
	}
	if skipped > 0 {
		out += fmt.Sprintf("  … and %d more agent(s) beyond this poll's summary budget; poll again after this batch finishes.\n", skipped)
	}
	if snap.Dropped > 0 {
		out += fmt.Sprintf("  %d agent(s) were beyond the snapshot's own cap and are not represented here.\n", snap.Dropped)
	}
	return out, nil
}

// snapshotBlock renders one agent: a head line with the identity, state, age,
// and call counts, then the last call, the trailing trail, the file set, and
// any note. One line per item keeps the batch scannable.
func snapshotBlock(a supervisionapi.AgentSnapshot, now time.Time) string {
	role := a.Role
	if role == "" {
		role = "(no role)"
	}
	if a.Label != "" {
		role += " [" + a.Label + "]"
	}
	state := "running"
	age := ""
	switch {
	case a.StillRunning && !a.StartedAt.IsZero():
		age = fmt.Sprintf("  age=%s", now.Sub(a.StartedAt).Round(time.Second))
	case !a.StillRunning && !a.StartedAt.IsZero() && !a.FinishedAt.IsZero():
		state = "finished"
		age = fmt.Sprintf("  ran=%s", a.FinishedAt.Sub(a.StartedAt).Round(time.Second))
	case !a.StillRunning:
		state = "finished"
	}
	block := fmt.Sprintf("  %s  scope=%s  %s%s  calls=%d errors=%d", role, a.ScopeID, state, age, a.Calls, a.Errors)
	if a.Through >= 0 {
		block += fmt.Sprintf("  through=%d", a.Through)
	}
	block += "\n"
	if a.Workdir != "" {
		block += "    workdir: " + a.Workdir + "\n"
	}
	if a.LastAction != "" {
		block += "    last: " + a.LastAction
		if a.LastArgs != "" {
			block += " " + a.LastArgs
		}
		block += "\n"
	}
	if len(a.Trail) > 0 {
		block += "    trail: " + snapshotTrail(a.Trail) + "\n"
	}
	if len(a.ModifiedFiles) > 0 {
		files := ""
		for i, f := range a.ModifiedFiles {
			if i > 0 {
				files += ", "
			}
			files += f
		}
		block += "    files: " + files + "\n"
	}
	if a.Note != "" {
		block += "    note: " + a.Note + "\n"
	}
	return block
}

// snapshotTrail renders the trailing calls oldest-first, marking the ones
// whose capture was not seen and the ones that failed. Args stay off this
// line — the last call's args are already above — so a long trail stays one
// readable line.
func snapshotTrail(trail []supervisionapi.ToolCallDigest) string {
	line := ""
	for i, c := range trail {
		if i > 0 {
			line += ", "
		}
		line += c.Name
		switch {
		case !c.Paired:
			line += " (capture pending)"
		case !c.Success:
			line += " ERR"
			if c.Error != "" {
				line += "(" + c.Error + ")"
			}
		}
	}
	return line
}
