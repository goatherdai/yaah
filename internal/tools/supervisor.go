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
	// Redirect CAS-claims the scope's current generation for a handoff,
	// carrying the complete payload (guidance plus the live-revert mark)
	// in ONE publication: single winner per generation
	// (ErrAlreadyClaimed), ErrNoRun when the generation no longer names a
	// live run or has passed its final adoption check.
	Redirect(scopeID string, fromGen uint64, req supervisionapi.RedirectRequest) (supervisionapi.RedirectHandle, error)
	// CancelGeneration ends exactly the claimed generation — the
	// redirect's stop. A scope-keyed Cancel could stop a successor that
	// already carries the guidance (PR #398 review finding).
	CancelGeneration(scopeID string, gen uint64) (<-chan struct{}, error)
}

// firingSource is the status view's consumer-side view of the advisory
// rules service.
type firingSource interface {
	Firings() []supervisionapi.Firing
}

// interventionSink is the OPTIONAL record half of the run bridge: a bridge that
// keeps the session's intervention log accepts the records a root action makes,
// so the run the action touched carries them into its dispatch envelope
// (yaah-vaxv, design §4.10). It is asserted for rather than declared on
// runBridge: a fake bridge, or any wiring that predates the log, still
// satisfies runBridge and simply records nothing.
type interventionSink interface {
	RecordInterventions(recs ...supervisionapi.InterventionRecord)
}

// haltGate is the OPTIONAL half of the run bridge a stop's record needs: a
// bridge that can say whether a cancel will reach a run lets this tool record
// the stop BEFORE cancelling it. That order is not a nicety — the cancel is what
// makes the run settle, and a settling run drains the records for its own
// envelope, so a stop recorded after the cancel is missing from precisely the
// envelope it belongs in (PR #415 review finding 2). A bridge without it records
// after the halt, as this tool always did.
type haltGate interface {
	Haltable(scopeID string) bool
}

// recordRootAction appends one action the dispatching agent took through this
// tool, with Source = "root" — the third source in the record vocabulary, and
// the only one that is not the policy loop or a rule.
//
// Tick is 0: a human- or orchestrator-driven action is outside the tick loop.
// gen is the incarnation the action targeted, so a record and the redirect claim
// it came from name the same run.
//
// The reason and guidance are bounded here exactly as the Service and the arbiter
// bound theirs (supervisionapi.TruncateWords): both are model-authored text that
// travels into the trace fact and the dispatch envelope, and a producer that
// skipped the bound would let one tool call put unbounded prose in either —
// while disagreeing with the envelope's own clip of the same field.
func recordRootAction(runs runBridge, scopeID string, action supervisionapi.Action, reason, guidance, outcome, dropReason string, gen uint64) {
	sink, ok := runs.(interventionSink)
	if !ok || portIsNil(sink) {
		return
	}
	sink.RecordInterventions(supervisionapi.InterventionRecord{
		At:         time.Now(),
		Tick:       0,
		Scope:      scopeID,
		Source:     supervisionapi.SourceRoot,
		Action:     action,
		Reason:     supervisionapi.TruncateWords(reason, supervisionapi.MaxReasonWords),
		Guidance:   supervisionapi.TruncateWords(guidance, supervisionapi.MaxGuidanceWords),
		Outcome:    outcome,
		DropReason: dropReason,
		Generation: gen,
	})
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
		if raw.ScopeID == t.RootScopeID {
			// The root registration has no adoption loop: redirecting it
			// cancels the session's run, promises a replacement that
			// never starts, and leaves the claim jamming later redirects
			// (PR #398 review finding). Unconditional — the halt_root
			// opt-in gates halt only.
			return "", fmt.Errorf("supervisor: the session root cannot be redirected or reverted: no dispatch loop exists to adopt a replacement. Use halt (with halt_root) to stop the session, or inject to steer it")
		}
		return executeRedirect(mgr, runs, raw.ScopeID, raw.Guidance, false)
	case "revert":
		if raw.ScopeID == "" || raw.Guidance == "" {
			return "", fmt.Errorf("supervisor: scope_id and guidance required for revert")
		}
		if raw.ScopeID == t.RootScopeID {
			// The root registration has no adoption loop: reverting it
			// cancels the session's run, promises a replacement that never
			// starts, and leaves the claim jamming later redirects (PR #398
			// review finding). Unconditional — the halt_root opt-in gates
			// halt only.
			return "", fmt.Errorf("supervisor: the session root cannot be redirected or reverted: no dispatch loop exists to adopt a replacement. Use halt (with halt_root) to stop the session, or inject to steer it")
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
	var gen uint64
	if runs != nil {
		gen, _ = runs.Generation(scopeID)
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
		recordRootAction(runs, scopeID, supervisionapi.ActionSteer, injectReason, guidance, supervisionapi.OutcomeApplied, "", gen)
		return fmt.Sprintf("Injected guidance into %s; delivered to the running loop.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrNoRun):
		recordRootAction(runs, scopeID, supervisionapi.ActionSteer, injectReason, guidance, supervisionapi.OutcomeDropped, supervisionapi.DropReasonNoRun, gen)
		return fmt.Sprintf("Guidance recorded in the trace for %s; no active run to deliver it to (the scope is not currently executing).", scopeID), nil
	case errors.Is(err, supervisionapi.ErrSteerBusy):
		recordRootAction(runs, scopeID, supervisionapi.ActionSteer, injectReason, guidance, supervisionapi.OutcomeDropped, supervisionapi.DropReasonSteerBusy, gen)
		return fmt.Sprintf("Guidance recorded in the trace for %s, but the run's steering buffer is full — it is not consuming guidance; consider halting the scope instead.", scopeID), nil
	default:
		return "", fmt.Errorf("supervisor: inject delivery failed: %w", err)
	}
}

// injectReason is the record's reason for an inject this tool performed: the
// action is the dispatching agent's own, not a rule's or the policy loop's, and
// the guidance is what it asked for.
const injectReason = "supervisor tool inject"

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
	//
	// The stop's RECORD is the one thing that goes before the cancel: the
	// cancel is what makes the halted run settle, and a settling run drains
	// the records for its own envelope, so a record made after it can only
	// linger in the log (PR #415 review finding 2). Haltable asks the same
	// question Cancel is about to ask, so a refusal still records a drop.
	var cancelled, quiesced, waited, recorded bool
	var gen uint64
	if runs != nil {
		gen, _ = runs.Generation(scopeID)
		if gate, ok := runs.(haltGate); ok && gate.Haltable(scopeID) {
			recordRootAction(runs, scopeID, supervisionapi.ActionStop, haltReason, "", supervisionapi.OutcomeApplied, "", gen)
			recorded = true
		}
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
		if !recorded {
			recordRootAction(runs, scopeID, supervisionapi.ActionStop, haltReason, "", supervisionapi.OutcomeApplied, "", gen)
		}
		return fmt.Sprintf("Halted %s; the running loop was cancelled and has stopped.", scopeID), nil
	case cancelled:
		if !recorded {
			recordRootAction(runs, scopeID, supervisionapi.ActionStop, haltReason, "", supervisionapi.OutcomeApplied, "", gen)
		}
		return fmt.Sprintf("Halted %s; the run was cancelled but had not stopped within %s — an in-flight tool may have been interrupted by the sandbox teardown.", scopeID, haltQuiesceTimeout), nil
	default:
		// The kernel-side halt landed with no run behind it, which is the
		// honest "nothing was cancelled" case: recorded as a drop so the
		// scope's envelope does not claim a run was stopped. (recorded is
		// false here by construction: Haltable said no run to cancel.)
		if !recorded {
			recordRootAction(runs, scopeID, supervisionapi.ActionStop, haltReason, "", supervisionapi.OutcomeDropped, supervisionapi.DropReasonNoRun, gen)
		}
		return fmt.Sprintf("Halted %s in the trace; no active run to cancel (the scope is not currently executing).", scopeID), nil
	}
}

// haltReason is the record's reason for a stop this tool performed.
const haltReason = "supervisor tool halt"

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
	// The claim carries the complete payload in ONE registry publication
	// (PR #398 review finding: the old empty-slot claim let the parent
	// loop consume an empty request before the guidance landed).
	recordAction := supervisionapi.ActionRedirect
	if revert {
		recordAction = supervisionapi.ActionRevert
	}
	handle, err := runs.Redirect(scopeID, gen, supervisionapi.RedirectRequest{Guidance: guidance, Revert: revert})
	switch {
	case err == nil:
	case errors.Is(err, supervisionapi.ErrAlreadyClaimed):
		recordRootAction(runs, scopeID, recordAction, rootReason(revert), guidance, supervisionapi.OutcomeDropped, supervisionapi.DropReasonAlreadyClaimed, gen)
		return fmt.Sprintf("A handoff is already claimed and in progress for %s; nothing was claimed. Let the current handoff finish, or use halt to stop the run.", scopeID), nil
	case errors.Is(err, supervisionapi.ErrNoRun):
		// The inspected generation finished (or was superseded, or passed
		// its final adoption check) between the generation read and the
		// claim: the race is a normal inspection-to-claim gap, not a
		// fault (PR #400 review finding 1). Report it as an outcome so
		// the supervisor re-inspects instead of taking a tool failure.
		recordRootAction(runs, scopeID, recordAction, rootReason(revert), guidance, supervisionapi.OutcomeDropped, supervisionapi.DropReasonNoRun, gen)
		return fmt.Sprintf("No active run on %s to hand off — the run inspected finished or was replaced before it could be claimed; nothing was claimed. Re-inspect the scope (list_scopes) and redirect again if a new run is executing.", scopeID), nil
	default:
		return "", fmt.Errorf("supervisor: redirect claim failed: %w", err)
	}
	what := "guidance"
	verb := "Redirect requested"
	if revert {
		what = "revert plus guidance"
		verb = "Revert requested"
	}
	// The claim landed: the record says claimed, the outcome word the arbiter
	// uses for a handoff whose adoption is still to come.
	recordRootAction(runs, handle.ScopeID, recordAction, rootReason(revert), guidance, supervisionapi.OutcomeClaimed, "", handle.Gen)
	// Stop the claimed generation, generation-checked: if it already
	// finished and a successor registered, that replacement carries the
	// guidance and must NOT be cancelled (PR #398 review finding).
	if _, err := runs.CancelGeneration(handle.ScopeID, handle.Gen); err != nil {
		return fmt.Sprintf("%s for %s (generation %d), but the run had already finished: if a replacement started it carries the %s; nothing else was cancelled.", verb, scopeID, handle.Gen, what), nil
	}
	return fmt.Sprintf("%s for %s (generation %d): the run is stopping and a replacement loop starts with %s when it returns; a supervisor halt in that window supersedes the handoff.", verb, scopeID, handle.Gen, what), nil
}

// rootReason is the record's reason for a handoff this tool requested: the
// dispatching agent's own action, named as such so a reader can tell it from a
// policy decision on the same scope.
func rootReason(revert bool) string {
	if revert {
		return "supervisor tool revert"
	}
	return "supervisor tool redirect"
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
