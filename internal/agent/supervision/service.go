// Service bridges the kernel's advisory supervision rules to the D2
// execution bridge (goatherd-adoption plan B1): the kernel supervisor
// watches the trace's effect bus, and this service drains its
// interventions into the run registry, so a rule firing becomes a
// delivered steering message in the running loop — not just a trace
// fact.
//
// The service is advisory-only by construction (B1): it consumes
// InterventionInject and drops every other intervention type — the
// blocking destructive guard is B2's synchronous path, and rule-driven
// halts are B3's.
package supervision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	"github.com/goatherdai/goatherd-runtime"
	supervisor "github.com/goatherdai/goatherd-runtime/supervisor"

	"github.com/goatherdai/yaah/internal/effectpolicy"
	"github.com/goatherdai/yaah/internal/permissions"
	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/types"
)

// AdvisoryRules carries the advisory rules' switches and tuning.
// Zero thresholds mean the documented defaults (0.5 error rate over a
// window of 10 — the window's zero-default lives in the kernel — and a
// repeat threshold of 3); this struct only transports configuration.
type AdvisoryRules struct {
	// DestructiveTool enables the advisory destructive-declaration
	// warning rule.
	DestructiveTool bool

	// HighErrorRate enables the error-rate guidance rule.
	HighErrorRate bool

	// ErrorRateThreshold and ErrorRateWindow carry the rule's tuning;
	// zero threshold means 0.5, zero window the kernel's own 10.
	ErrorRateThreshold float64
	ErrorRateWindow    int

	// StuckDetection enables the repeated-call guidance rule.
	StuckDetection bool

	// StuckRepeatThreshold carries the rule's tuning; zero means 3.
	StuckRepeatThreshold int

	// ShellGuardAllowCommands is the global default allowlist for the
	// blocking shell guard (B2): shell commands that may dispatch, as
	// patterns matched in full against the raw command. Empty is
	// deny-all — the guard's historical default-deny for the shell.
	// A dispatching role's own allowlist replaces this list.
	ShellGuardAllowCommands []string
}

// Any reports whether at least one advisory rule is on — the wiring's
// cheap answer to "is there anything to supervise with".
func (r AdvisoryRules) Any() bool {
	return r.DestructiveTool || r.HighErrorRate || r.StuckDetection
}

// Firing is one counted rule firing against one scope (adoption plan
// B4): the `supervisor status` surface lists these, so the
// orchestrator can see which rules acted this session without reading
// logs. Kind is the intervention type as fired (inject, halt, deny).
// A deny carries its detail — the refused command and the reason the
// model was given — in Command and Reason, so the line names what was
// blocked and why. The type lives in supervisionapi so the supervisor
// tool can render it without importing this package.
type Firing = supervisionapi.Firing

// firingKey aggregates firings per rule, scope, and kind — and, for a
// blocking deny, per refused command and reason, so the status line
// names what was blocked and why rather than only how often. Advisory
// firings carry neither and keep their old aggregation.
type firingKey struct{ rule, scope, kind, command, reason string }

// Service owns the kernel supervisor for one session and the drain
// goroutine that turns its interventions into steering.
type Service struct {
	sup         *supervisor.Supervisor
	guard       *supervisor.Supervisor
	mgr         *goatherd.ScopeManager
	runs        *Registry
	bus         *kerneltrace.EffectBus
	rootScopeID string
	haltRoot    bool

	// firings counts rule firings for `supervisor status` (adoption
	// plan B4). Recorded where the rule name is known — the advisory
	// wrapper for the engine's rules, the guard closure for its denies
	// — at the moment the rule acts, before any dispatch policy is
	// applied: a firing dropped against the root or refused by the
	// halt gate still fired, and status says so.
	firingsMu sync.Mutex
	firings   map[firingKey]int

	// mu guards the lifecycle transitions: wg.Add in Start must never
	// race wg.Wait in Close — the WaitGroup reuse rule — so both hold
	// the mutex through their transition, and closed makes Start a
	// no-op once Close has begun.
	mu      sync.Mutex
	closed  bool
	started bool
	wg      sync.WaitGroup
}

// GuardCheck returns the blocking pre-dispatch check for sub-agent
// loops (adoption plan B2): the sub-agent pipeline consults it before
// a tool call executes, and a non-empty reason denies the call. Nil
// when the shell guard is disabled in config — a nil check is the
// pass-through, so tracing-off sessions are byte-identical to today.
//
// The event mirrors the declaration the trace middleware records —
// schema ref, kind label, and the shell command under "cmd" — so the
// rule set evaluates pre-dispatch and post-hoc against one shape.
//
// Safe under concurrent sub-agent loops: the kernel's CheckCall copies
// the rule slice under a read lock, and the guard rule is stateless —
// both closures read only the event — so concurrent dispatches share
// one check without shared mutable state.
func (s *Service) GuardCheck() func(ownerID, toolName, argsJSON string) string {
	if s.guard == nil {
		return nil
	}
	return func(ownerID, toolName, argsJSON string) string {
		payload := map[string]any{
			"tool": toolName,
			"args": json.RawMessage(argsJSON),
		}
		if shellGuardTool(toolName) {
			if cmd := toolCommand(argsJSON); cmd != "" {
				payload["cmd"] = cmd
			}
		}
		iv := s.guard.CheckCall(kerneltrace.EffectEvent{
			TraceOwnerID: ownerID,
			Mode:         kerneltrace.Declaration,
			SchemaRef:    "yaah.tool." + toolName + ".v1",
			KindLabel:    toolName,
			Payload:      payload,
		})
		if iv == nil || iv.Type != supervisor.InterventionDeny {
			return ""
		}
		// Fail closed: a deny verdict denies regardless of whether the
		// payload type-asserts or carries text. A silent empty string
		// here would turn a non-string (or empty) payload into an
		// allow — fail-open in the one pre-dispatch net sub-agent
		// calls pass through.
		reason, ok := iv.Payload.(string)
		if !ok || reason == "" {
			reason = "Blocked destructive operation. Supervisor denied."
		}
		// The deny records what was refused (the command under the same
		// "cmd" key the declaration carries) and why, beside the count
		// and as a durable fact on the refused scope. Recording cannot
		// move the verdict: the reason returns below either way.
		cmd, _ := payload["cmd"].(string)
		s.recordDenial(shellGuardRuleName, iv.ScopeID, string(iv.Type), cmd, reason)
		s.appendDenyFact(iv.ScopeID, shellGuardRuleName, toolName, cmd, reason)
		return reason
	}
}

// advisoryRule adapts a kernel rule for the session's engine with the
// three invariants the plan holds:
//
//  1. Supervisor records never feed it — the drain records each firing
//     in the trace, the store publishes that append, and the stuck rule
//     matches ALL declarations: without the filter, one firing feeds
//     the next in an endless loop.
//  2. Its guidance names its source (adoption plan B3: the guidance
//     text includes the rule), so the loop being steered — and the
//     trace holding the fact — can tell an error-rate warning from a
//     stuck-loop warning apart.
//  3. Each firing is counted (adoption plan B4): onFiring observes the
//     intervention at the moment the rule acts — before dispatch
//     policy, so a root-dropped or gate-refused firing still counts —
//     keyed by the rule's name for `supervisor status`.
func advisoryRule(rule supervisor.SupervisionRule, onFiring func(rule string, iv *supervisor.Intervention)) supervisor.SupervisionRule {
	innerMatch := rule.Match
	innerAction := rule.Action
	rule.Match = func(e kerneltrace.EffectEvent) bool {
		if e.SchemaRef == goatherd.SchemaSupervisorInject {
			return false
		}
		return innerMatch(e)
	}
	rule.Action = func(e kerneltrace.EffectEvent) *supervisor.Intervention {
		iv := innerAction(e)
		if iv == nil {
			return nil
		}
		if onFiring != nil {
			onFiring(rule.Name, iv)
		}
		if payload, ok := iv.Payload.(string); ok {
			iv.Payload = fmt.Sprintf("[%s] %s (observed: %s)", rule.Name, payload, e.KindLabel)
		}
		return iv
	}
	return rule
}

// The advisory rules' documented defaults (plan §7). The kernel's rule
// constructors default only the error-rate window — a zero repeat
// threshold makes the stuck rule fire on every declaration, and a zero
// error-rate threshold on every half-window — so the zeros of an
// unconfigured stanza resolve here.
const (
	defaultErrorRateThreshold = 0.5
	defaultStuckRepeat        = 3
)

// haltQuiesceTimeout bounds the drain's wait for a halted run to stop
// before the kernel-side halt lands (adoption plan B3), mirroring the
// A3 manual halt's own bound (internal/tools/supervisor.go — the value
// is duplicated rather than shared because tools imports this package,
// not the other way around).
const haltQuiesceTimeout = 10 * time.Second

// destructiveFileRule is yaah's advisory destructive_file_guard. The
// kernel's DestructiveToolRule flags every write declaration ("all writes
// are potentially destructive"), which warns on plain new-file creation —
// the common case in a sub-agent worktree — and trains warning fatigue
// (yaah-0fn). yaah keeps the kernel's bash blacklist and delete match but
// classifies a write by what the tool observed: file_existed=true is an
// overwrite (warn), false is first-time creation (pass), and a missing
// field — a tool that attached nothing, or a Stat error other than
// not-exist — stays conservative and warns.
func destructiveFileRule() supervisor.SupervisionRule {
	kernel := supervisor.DestructiveToolRule()
	return supervisor.SupervisionRule{
		// The kernel's name, read from the rule itself: it is
		// user-visible via `supervisor status` and must not diverge.
		Name:   kernel.Name,
		Match:  destructiveMatch(kernel.Match),
		Action: kernel.Action,
	}
}

// destructiveMatch wraps a kernel matcher with yaah's write
// classification.
func destructiveMatch(kernelMatch func(kerneltrace.EffectEvent) bool) func(kerneltrace.EffectEvent) bool {
	return func(e kerneltrace.EffectEvent) bool {
		if e.Mode != kerneltrace.Declaration {
			return false
		}
		if e.SchemaRef == types.ToolSchemaRef("write") || e.KindLabel == "write" {
			existed, ok := e.Payload[types.SupervisionKeyFileExisted].(bool)
			return !ok || existed
		}
		return kernelMatch(e)
	}
}

// SchemaShellGuardDeny is the trace fact one blocked shell call appends
// (adoption plan B2): a Capture record on the refused sub-agent's own
// owner path naming the rule, the tool, the command, and the reason it
// was refused with. It sits beside the kernel's goatherd.supervisor.*
// records under a schema ref of its own, so an existing trace reader
// keeps working unchanged.
const SchemaShellGuardDeny = "yaah.supervisor.shell_guard.deny.v1"

// ShellGuardDenyKind is the deny fact's kind label, in the same
// "supervisor:*" label family Scope.Inject writes.
const ShellGuardDenyKind = "supervisor:deny"

// shellGuardRuleName is the guard's rule name, and so the name its
// denials are counted and recorded under: one constant, because the rule
// a trace names and the rule status counts must never diverge.
const shellGuardRuleName = "destructive_shell_guard"

// shellAllowlistRuleName is the role-allowlist deny's rule name, and so
// the name those refusals are counted and recorded under. It is
// deliberately distinct from shellGuardRuleName: the global guard and a
// role's own allow_commands list are different authorities, and a status
// line or a trace fact that named either as the other would misreport
// which one refused.
const shellAllowlistRuleName = "role_shell_allowlist"

// scopeIDPrefix is how the scope manager keys a scope: "scope:" plus the
// scope's trace owner. A deny recorded from a sub-agent loop knows only
// the owner, so it needs the prefix to address the same scope the
// kernel's own denies name.
const scopeIDPrefix = "scope:"

// shellDenyKind is the status-surface kind both deny paths count under —
// the kernel's deny intervention rendered as the string the global guard
// already records (`string(iv.Type)`) — so a role-allowlist refusal
// aggregates exactly like a global one instead of inventing a second
// spelling of "deny".
var shellDenyKind = string(supervisor.InterventionDeny)

// shellGuardRule composes the blocking shell guard from the kernel's
// DestructiveToolGuard (adoption plan B2, decision D3: "default-deny
// for bash"), reusing the kernel's deny action verbatim. The kernel's
// own matcher block-matches bash, write, and delete — the plan's
// settled scope is the shell only (both of yaah's: bash on POSIX,
// powershell on Windows, which the kernel's matcher has never heard
// of), because structured tools are the approved path for changes and
// their review is the merge gate's job (Phase C), not this net.
//
// background_process joins the shells for its command-running action
// only: it executes an arbitrary command through the host process
// manager — the same "a shell can execute anything a pattern matcher
// cannot see" hazard, in a second door. Its read-only actions
// (status, output) and stop pass: a command is the hazard, and only
// the start action carries one.
//
// plan is the guard's authority surface (shellGuardPlan): a command
// matching one of the allowlist globs dispatches, everything else is
// denied. An empty allowlist compiles to a plan that denies every shell
// call — the historical default-deny — so the rule never inspects the
// command it cannot admit.
func shellGuardRule(plan effectpolicy.Plan) supervisor.SupervisionRule {
	guard := supervisor.DestructiveToolGuard()
	return supervisor.SupervisionRule{
		Name: shellGuardRuleName,
		Match: func(e kerneltrace.EffectEvent) bool {
			if e.Mode != kerneltrace.Declaration {
				return false
			}
			// The command arrives under "cmd" (GuardCheck's payload
			// shape, the same key the trace middleware stamps). A
			// shell call carrying none is still a guarded effect:
			// it matches no allowlist pattern and is denied.
			cmd, _ := e.Payload["cmd"].(string)
			eff, guarded := shellGuardEffect(e.KindLabel, cmd)
			if !guarded {
				return false
			}
			return plan.Check(eff) != nil
		},
		Action: guard.Action,
	}
}

// shellGuardPlan compiles a shell-command allowlist into the guard's
// authority plan: deny every shell effect whose command does not match one
// of the allowlist globs.
//
// The plan reads exactly that sentence. shellKinds is the guarded surface
// — both shells plus background_process's command-running action — and
// allowedCmd is the disjunction of one command atom per allowlist pattern,
// each carrying the anchored expression permissions.GlobRegexp derives from
// the same glob permissions.GlobMatch matches. Denying
// shellKinds.And(allowedCmd.Not()) permits a call exactly when it is not a
// guarded shell, or is one whose command matches: an empty allowlist leaves
// allowedCmd as Nothing(), whose complement is All(), so every shell call
// falls under the deny.
func shellGuardPlan(allow []string) effectpolicy.Plan {
	shellKinds := effectpolicy.Exact("bash").
		Or(effectpolicy.Exact("powershell")).
		Or(effectpolicy.Exact("background_process"))
	allowedCmd := effectpolicy.Nothing()
	for _, pattern := range allow {
		expr, err := permissions.GlobRegexp(pattern)
		if err != nil {
			// An uninterpretable pattern admits nothing in
			// permissions.GlobMatch either: a rule that cannot be read
			// must not widen what its author wrote.
			continue
		}
		allowedCmd = allowedCmd.Or(effectpolicy.Field("command", effectpolicy.Matches, expr))
	}
	return effectpolicy.Plan{}.DenyKind(shellKinds.And(allowedCmd.Not()))
}

// shellGuardEffect classifies one call as the guard's effect: the tool's
// kind plus the command it carries. The second return is false when the call
// is not a guarded shell effect at all — a non-shell tool, or
// background_process's read-only actions, which carry no command and are the
// same pass-through as before.
//
// The command field carries the glob-normalized form of the command, because
// the plan's patterns are compiled from normalized globs; matching the
// normalized command is what reproduces permissions.GlobMatch's verdict
// exactly (a Windows `dir C:\x` that `dir C:/*` admits).
//
// A call with no command attaches no command field rather than an empty one:
// an absent field makes every atom over it false, so the plan denies — the
// guard's fail-closed reading of an unclassifiable call, and the same verdict
// permissions.GlobMatch gives the empty string, which no pattern (not even
// "*") admits.
func shellGuardEffect(kind, cmd string) (effectpolicy.Effect, bool) {
	switch kind {
	case "bash", "powershell":
	case "background_process":
		if cmd == "" {
			// Read-only actions carry no command: nothing to admit.
			return effectpolicy.Effect{}, false
		}
	default:
		return effectpolicy.Effect{}, false
	}
	eff := effectpolicy.Effect{Kind: kind}
	if cmd != "" {
		eff.Fields = map[string]any{"command": permissions.NormalizeGlobValue(cmd)}
	}
	return eff, true
}

// shellGuardTool reports whether the shell guard inspects this tool's
// command: both shells, plus background_process's command-running action
// (the same set GuardCheck stamps a "cmd" onto).
func shellGuardTool(toolName string) bool {
	switch toolName {
	case "bash", "powershell", "background_process":
		return true
	}
	return false
}

// toolCommand extracts the shell command a call carries under
// "command". Empty when it carries none — an unparseable payload, or
// background_process's read-only actions.
func toolCommand(argsJSON string) string {
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return ""
	}
	return args.Command
}

// WithShellAllowlist narrows a guard check to one role's shell-command
// allowlist. A sub-agent loop's owner ID is the trace session, not the
// role name, so the guard cannot resolve a role's allowlist when it
// denies — the dispatching runner resolves it (config role entry >
// role file > global default) and wraps the check here, alongside the
// DestructiveGuard it already passes into the loop.
//
// A nil allowlist inherits: the check is returned unchanged and the
// global default it was built with stays the authority. An explicitly
// empty list is deny-all, which is the pass-through it reads as — every
// shell call is refused with a reason naming the role's allowlist.
// Non-shell tools and background_process's read-only actions are the
// global check's business either way.
//
// rec, when wired, receives every refusal this wrapper makes, so a
// role's own allowlist is as legible after the fact as the global
// guard's: the reason still returns unchanged, and recording never
// moves the verdict. Omitting it is the pre-recorder shape — the deny
// stands and nothing is recorded.
func WithShellAllowlist(check func(ownerID, toolName, argsJSON string) string, allow []string, rec ...DenyRecorder) func(ownerID, toolName, argsJSON string) string {
	if allow == nil {
		return check
	}
	return WithShellAllowlistPlan(check, shellGuardPlan(allow), rec...)
}

// DenyRecorder records one refusal a guard wrapper made, so it stays
// legible afterwards on both surfaces a reader has: the status firing
// (scope, tool, command, reason) and a durable trace fact on the
// refused scope. scopeID is the owner the check addressed — the
// loop's trace session, not the kernel's scope address — which the
// recorder resolves to the scope the manager knows.
type DenyRecorder func(scopeID, tool, cmd, reason string)

// WithShellAllowlistPlan narrows a guard check to an already-compiled
// authority plan — the plan shellGuardPlan builds from a role's allowlist,
// and the same shape the global guard rule carries. A role's narrowing and
// the global default therefore run one matching mechanism
// (effectpolicy.Plan.Check) rather than two: a guarded shell effect the plan
// refuses is denied with a reason naming the role's allowlist, and everything
// the plan does not classify — structured tools, background_process's
// read-only actions — stays the wrapped check's business untouched.
//
// rec, when wired, records the refusal before the reason returns: both
// deny branches (no command to match, and a command outside the list)
// are refusals the status and trace must be able to name. An admitted
// command records nothing.
func WithShellAllowlistPlan(check func(ownerID, toolName, argsJSON string) string, plan effectpolicy.Plan, rec ...DenyRecorder) func(ownerID, toolName, argsJSON string) string {
	if check == nil {
		return nil
	}
	return func(ownerID, toolName, argsJSON string) string {
		cmd := toolCommand(argsJSON)
		eff, guarded := shellGuardEffect(toolName, cmd)
		if !guarded {
			return check(ownerID, toolName, argsJSON)
		}
		if plan.Check(eff) == nil {
			return ""
		}
		reason := fmt.Sprintf("command %q is not in this role's allow_commands allowlist", cmd)
		if cmd == "" {
			reason = "shell call carries no command to check against the role's allowlist"
		}
		recordAllowlistDeny(rec, ownerID, toolName, cmd, reason)
		return reason
	}
}

// recordAllowlistDeny hands one role-allowlist refusal to the recorder,
// when one was wired. An unset recorder is the backward-compatible
// shape: the deny holds, nothing is recorded, and the caller's reason
// is returned either way.
func recordAllowlistDeny(rec []DenyRecorder, ownerID, tool, cmd, reason string) {
	if len(rec) == 0 || rec[0] == nil {
		return
	}
	rec[0](ownerID, tool, cmd, reason)
}

// NewService assembles the kernel supervisor with the enabled advisory
// rules, and — when shellGuard is set — a second, never-started
// supervisor carrying the blocking shell guard for synchronous
// pre-dispatch checks (B2). The thresholds default inside the kernel
// rules when zero, so a bare AdvisoryRules{DestructiveTool: true, ...}
// needs no tuning knowledge.
//
// rootScopeID is the session's root scope (adoption plan D1): advisory
// injections against it are dropped — the parent's own calls already
// passed the approval middleware — but a HALT against it is the B3
// path: dispatched only when haltRoot is set (the supervisor.halt_root
// opt-in, default off — a rule halting the interactive session is a
// big hammer). The manual supervisor tool's root inject (A2) is
// unaffected either way.
func NewService(mgr *goatherd.ScopeManager, bus *kerneltrace.EffectBus, runs *Registry, rules AdvisoryRules, shellGuard, haltRoot bool, rootScopeID string) *Service {
	errorThreshold := rules.ErrorRateThreshold
	if errorThreshold <= 0 {
		errorThreshold = defaultErrorRateThreshold
	}
	stuckRepeat := rules.StuckRepeatThreshold
	if stuckRepeat <= 0 {
		stuckRepeat = defaultStuckRepeat
	}
	// A negative window would narrow the kernel's own <= 0 default
	// clause to exactly zero; zero keeps the kernel's default (10).
	errorWindow := rules.ErrorRateWindow
	if errorWindow < 0 {
		errorWindow = 0
	}

	sup := supervisor.NewSupervisor(mgr, bus)
	var guard *supervisor.Supervisor
	svc := &Service{sup: sup, mgr: mgr, runs: runs, bus: bus, rootScopeID: rootScopeID, haltRoot: haltRoot}

	if rules.DestructiveTool {
		sup.AddRule(advisoryRule(destructiveFileRule(), svc.recordFiring))
	}
	if rules.HighErrorRate {
		sup.AddRule(advisoryRule(supervisor.HighErrorRateRule(errorThreshold, errorWindow), svc.recordFiring))
	}
	if rules.StuckDetection {
		sup.AddRule(advisoryRule(supervisor.StuckDetectionRule(stuckRepeat), svc.recordFiring))
	}

	if shellGuard {
		// A second instance, never started: it must never subscribe to
		// the bus, or every deny would also surface as an async
		// intervention the advisory drain would have to drop. Its
		// CheckCall is the whole surface.
		guard = supervisor.NewSupervisor(mgr, nil)
		guard.AddRule(shellGuardRule(shellGuardPlan(rules.ShellGuardAllowCommands)))
	}
	svc.guard = guard
	return svc
}

// recordFiring counts one rule firing for the status surface (adoption
// plan B4).
func (s *Service) recordFiring(rule string, iv *supervisor.Intervention) {
	s.record(rule, iv.ScopeID, string(iv.Type), "", "")
}

// recordDenial counts one blocking deny and keeps its detail — the
// command that was refused and the reason the model was given — beside
// the count, on the same status surface (adoption plan B4's deny path).
func (s *Service) recordDenial(rule, scopeID, kind, command, reason string) {
	s.record(rule, scopeID, kind, command, reason)
}

// record aggregates one firing under its rule, scope, kind, command,
// and reason. Advisory firings leave the last two empty.
func (s *Service) record(rule, scopeID, kind, command, reason string) {
	s.firingsMu.Lock()
	if s.firings == nil {
		s.firings = map[firingKey]int{}
	}
	s.firings[firingKey{rule: rule, scope: scopeID, kind: kind, command: command, reason: reason}]++
	s.firingsMu.Unlock()
}

// appendDenyFact records one blocked call in the refused scope's trace:
// the durable half of the deny's legibility, so a post-hoc reader of the
// sub-agent's own trace sees the command the guard refused, the tool
// that carried it, and the reason given. It is a Capture fact under a
// schema ref of its own, so a reader of the kernel's
// goatherd.supervisor.* records is unaffected, and no rule matches it
// (the rules read declarations, except the error-rate rule, which reads
// captures carrying "success" — this payload has none).
//
// A scope the manager cannot address — a check for an owner that never
// forked, as a test or a hand-driven call has — records no fact: the
// firing still holds the detail, and a trace failure must never change
// the verdict.
//
// rule names the authority that refused (shellGuardRuleName or
// shellAllowlistRuleName), so one deny schema and one reader serve both
// paths while the payload still says which one acted.
func (s *Service) appendDenyFact(scopeID, rule, tool, command, reason string) {
	scope, ok := s.mgr.Get(scopeID)
	if !ok {
		slog.Debug("supervisor: shell guard deny not recorded — scope not addressable",
			"scope", scopeID)
		return
	}
	payload := map[string]any{
		"rule":   rule,
		"tool":   tool,
		"cmd":    command,
		"reason": reason,
	}
	if _, err := scope.AppendFact(context.Background(), SchemaShellGuardDeny, ShellGuardDenyKind, payload); err != nil {
		slog.Warn("supervisor: shell guard deny not recorded in the trace",
			"scope", scopeID, "err", err)
	}
}

// ShellAllowlistDenyRecorder returns the recorder a sub-agent loop's
// guard wrapper carries, so a role-allowlist refusal lands on the same
// two surfaces the global guard's does: the `supervisor status` firing
// (with the refused command and the reason beside the count) and a
// durable SchemaShellGuardDeny fact on the refused scope's own owner
// path, whose payload names shellAllowlistRuleName in its rule field.
//
// The wrapper knows only the loop's trace owner, while the global
// guard's deny already carries the kernel's scope address; the recorder
// resolves one to the other first, so both paths count and record under
// the same scope identity. A nil service returns a nil recorder, which
// every caller treats as "record nothing".
func (s *Service) ShellAllowlistDenyRecorder() DenyRecorder {
	if s == nil {
		return nil
	}
	return func(scopeID, tool, cmd, reason string) {
		scopeID = s.denyScopeID(scopeID)
		s.recordDenial(shellAllowlistRuleName, scopeID, shellDenyKind, cmd, reason)
		s.appendDenyFact(scopeID, shellAllowlistRuleName, tool, cmd, reason)
	}
}

// denyScopeID resolves a deny's scope to the manager's own key. The
// kernel hands the global guard "scope:<owner>"; the role wrapper has
// only the loop's trace owner, which is the same owner. Whichever form
// the manager can address wins, and an ID it cannot address comes back
// unchanged — the refusal is still counted, only the trace fact is
// skipped (appendDenyFact's own unaddressable-scope path).
func (s *Service) denyScopeID(scopeID string) string {
	if s.mgr == nil {
		return scopeID
	}
	if _, ok := s.mgr.Get(scopeID); ok {
		return scopeID
	}
	if qualified := scopeIDPrefix + scopeID; qualified != scopeID {
		if _, ok := s.mgr.Get(qualified); ok {
			return qualified
		}
	}
	return scopeID
}

// Firings returns the session's counted rule firings (adoption plan
// B4), sorted by rule then scope then kind then the deny detail — a
// deterministic snapshot for `supervisor status`. A deny carries the
// refused command and the reason with it; an advisory firing carries
// neither. Empty when no rule has acted.
func (s *Service) Firings() []Firing {
	s.firingsMu.Lock()
	defer s.firingsMu.Unlock()
	out := make([]Firing, 0, len(s.firings))
	for k, n := range s.firings {
		out = append(out, Firing{
			Rule:    k.rule,
			Scope:   k.scope,
			Kind:    k.kind,
			Count:   n,
			Command: k.command,
			Reason:  k.reason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// Start subscribes the supervisor to the bus and launches the drain
// goroutine. Idempotent, and a no-op after Close: a service that is
// closing cannot gain a new drain goroutine.
func (s *Service) Start(subscriberID string) {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	if s.bus == nil {
		// The kernel subscribes to nothing without a bus, so its
		// interventions channel would never close and the drain would
		// wedge Close's join. Nothing to supervise either way.
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	if err := s.sup.Start(subscriberID); err != nil {
		// Unreachable with a non-nil bus (the natural refusal — a nil
		// bus — is filtered above), but a refused start means the kernel
		// subscribed nothing: launch no drain, drop the join count, and
		// name the wiring damage.
		slog.Warn("supervisor: kernel start failed; the advisory bridge is down", "err", err)
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		s.wg.Done()
		return
	}
	go s.drain()
}

// Close stops the supervisor and waits for the drain goroutine to
// exit. The kernel closes the interventions channel on its way down,
// which is what ends the drain; Close is therefore also the join point
// tests use to assert no goroutine leaks. Idempotent, and makes any
// later Start a no-op.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.sup.Close()
	s.wg.Wait()
}

// drain bridges interventions to execution. For each inject, the
// A2 order holds: the trace fact is the record, the registry delivery
// is the intervention — so the fact is written first (scope.Inject),
// then the guidance is delivered (runs.Inject). Every delivery
// outcome is honest in the log: a scope with no running loop keeps
// its recorded firing, and a full steering buffer names the wedged
// run instead of pretending delivery happened.
func (s *Service) drain() {
	defer s.wg.Done()

	for iv := range s.sup.Interventions() {
		// The kernel rules address scopes as "scope:<owner>" — which is
		// exactly the form the runner registers under (Scope.ID()) and
		// the manager keys on, so the intervention's ScopeID addresses
		// both bridges unchanged.
		scopeID := iv.ScopeID
		guidance, ok := iv.Payload.(string)
		if !ok {
			slog.Warn("supervisor: dropping intervention with non-string payload",
				"type", string(iv.Type), "scope", scopeID)
			continue
		}

		switch iv.Type {
		case supervisor.InterventionInject:
			if s.rootScopeID != "" && scopeID == s.rootScopeID {
				// The parent's own calls already passed the approval
				// middleware; rule-driven session steering is out of
				// B1's scope (and the plan's exit criterion is
				// explicit: the parent session stays untouched). The
				// firing is deliberately not recorded — the trace fact
				// is the record of a delivered intervention, and this
				// one is dropped by policy, not lost in transit.
				slog.Debug("supervisor: rule firing against the root scope dropped",
					"scope", scopeID)
				continue
			}
			s.deliverInject(scopeID, guidance)
		case supervisor.InterventionHalt:
			s.deliverHalt(scopeID, guidance)
		default:
			// The enabled rules emit injects and (from B3) the drain
			// dispatches halts. A deny is B2's synchronous path and
			// cannot arrive here; a dropped unknown type must not be
			// silent.
			slog.Warn("supervisor: dropping non-advisory intervention",
				"type", string(iv.Type), "scope", scopeID)
		}
	}
}

// deliverHalt dispatches a rule-driven halt (adoption plan B3) with the
// A3 ordering: cancel the running loop FIRST, wait — bounded — for it
// to quiesce, and only then record the kernel-side halt, which tears
// down an owned sandbox. Cancelling alone is not quiescence: an
// in-flight tool can still be inside the very sandbox scope.Halt
// destroys next.
//
// The root scope is the opt-in gate: a rule halting the interactive
// session is a big hammer, dispatched only under supervisor.halt_root
// (default off). A root halt with the gate off is refused and logged —
// the session keeps running.
func (s *Service) deliverHalt(scopeID, reason string) {
	if s.rootScopeID != "" && scopeID == s.rootScopeID && !s.haltRoot {
		slog.Warn("supervisor: rule-driven halt of the root scope refused — set supervisor.halt_root to allow it",
			"scope", scopeID)
		return
	}
	if scope, ok := s.mgr.Get(scopeID); ok && scope.State() == goatherd.ScopeMerged {
		// Merged is terminal: the kernel refuses a halt beside the merge
		// record. Skip before touching any run — cancelling a run for a
		// scope the kernel would refuse to halt is a side effect with no
		// halt to show for it.
		slog.Info("supervisor: halt skipped — scope already merged (terminal)", "scope", scopeID)
		return
	}

	var quiesced, waited bool
	if quiesce, err := s.runs.Cancel(scopeID); err == nil {
		if quiesce != nil {
			waited = true
			select {
			case <-quiesce:
				quiesced = true
			case <-time.After(haltQuiesceTimeout):
				// Bounded proceed, the A3 semantics: the kernel halt
				// still lands — halt-while-running is its meaning —
				// and the log names the unquiesced run.
			}
		}
	} else if errors.Is(err, ErrNotHaltable) {
		// A registered run that cannot be cancelled is still using
		// the scope's sandbox: the kernel-side halt would destroy it
		// underneath a live run — the exact teardown hazard the
		// cancel-first ordering exists to prevent. The manual halt
		// tolerates this (a human asked); a rule must not.
		slog.Warn("supervisor: halt refused — the run registered no cancellation", "scope", scopeID)
		return
	} else if !errors.Is(err, ErrNoRun) {
		// A delivery failure the registry did not classify is a
		// bridge bug; report it rather than half-halt.
		slog.Warn("supervisor: halt delivery failed", "scope", scopeID, "err", err)
		return
	}
	if scope, ok := s.mgr.Get(scopeID); ok {
		if err := scope.Halt(); err != nil {
			slog.Warn("supervisor: kernel halt failed", "scope", scopeID, "err", err)
			return
		}
	}
	switch {
	case waited && !quiesced:
		slog.Warn("supervisor: halted scope; the run was cancelled but had not stopped within the bound",
			"scope", scopeID, "reason", reason)
	default:
		slog.Info("supervisor: halted scope", "scope", scopeID, "reason", reason)
	}
}

// deliverInject writes the firing into the scope's trace, then steers
// the run if one is registered under the scope. The fact is the record;
// the delivery is the intervention — so a firing that cannot be
// recorded is NOT delivered: a delivery with no trace fact would read
// as an intervention that never happened, the exact inversion of the
// A2 contract. The practical cases are benign races — a scope halted
// mid-firing refuses injects, and a scope missing from the manager
// cannot be addressed at all — and in both the run is over, so there
// is nothing left to steer anyway.
func (s *Service) deliverInject(scopeID, guidance string) {
	scope, ok := s.mgr.Get(scopeID)
	if !ok {
		slog.Warn("supervisor: firing not delivered — scope not addressable", "scope", scopeID)
		return
	}
	if err := scope.Inject(guidance); err != nil {
		slog.Warn("supervisor: firing not delivered — not recorded in the trace",
			"scope", scopeID, "err", err)
		return
	}
	switch err := s.runs.Inject(scopeID, guidance); {
	case err == nil:
		slog.Info("supervisor: guidance delivered", "scope", scopeID, "guidance", guidance)
	case errors.Is(err, ErrNoRun):
		// The firing is recorded in the trace; the scope simply has no
		// loop to steer right now (finished, or a root between runs).
		slog.Debug("supervisor: firing recorded, no active run", "scope", scopeID)
	case errors.Is(err, ErrSteerBusy):
		slog.Warn("supervisor: run not consuming guidance; consider halting the scope",
			"scope", scopeID)
	default:
		slog.Warn("supervisor: inject delivery failed", "scope", scopeID, "err", err)
	}
}
