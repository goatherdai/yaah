package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/goatherdai/yaah/internal/tracestore"
)

// SupervisorConfig configures the supervision rules engine
// (goatherd-adoption plan B1). Tracing is integral, so the stanza is
// always consulted.
//
// Defaults, precisely: with tracing on and the stanza absent, every
// advisory rule is enabled with its built-in thresholds. Disables and
// thresholds are therefore expressed as deviations — a nil rule switch
// means enabled, a zero threshold means the rule's default.
type SupervisorConfig struct {
	// Enabled is the master switch for the advisory rules engine. Nil
	// (key unset) means on: tracing is already the opt-in, and the rules
	// exist to make supervision automatic. Explicit false disables the
	// whole engine — no rule fires, no intervention is bridged.
	//
	// An EXPLICIT true also turns on supervised dispatch (design §4.2,
	// yaah-ugtl): a dispatch whose lane routing would run it on the shared
	// workspace is upgraded to the isolated report lane, and a failed
	// pre-run capture fails that dispatch at start instead of warning. That
	// consequence is why only an explicit true counts — the documented
	// default is on for the advisory rules, and reading the default as
	// "supervise every dispatch" would upgrade every shared-lane dispatch
	// for sessions that never asked (design D5). RequireReversible narrows
	// it further; see docs/configuration.md.
	Enabled *bool `yaml:"enabled"`

	// HaltRoot opts into supervisor halt of the session's root scope: a
	// deliberate stop of the session's current run, the ctrl-C
	// equivalent. Nil (unset) falls back to the A3 legacy flat key
	// (Defaults.SupervisorHaltRoot) — see HaltRootEnabled; an explicit
	// value wins over the legacy key in both directions. Off by
	// default everywhere — a rules engine or model halting the
	// interactive session is a big hammer. Root inject needs no gate:
	// steering the session is reversible.
	HaltRoot *bool `yaml:"halt_root"`

	// RequireReversible refuses a supervised dispatch whose workspace it
	// cannot roll back (design §4.2): a scope that owns no isolated
	// workspace — a container session, a shared root — fails at start
	// instead of running with Reversible false. Off by default, so an
	// unrevertable dispatch proceeds exactly as it did before; the
	// measurement sets it (design §7). It only ever narrows a supervised
	// dispatch: with supervision off nothing consults it.
	RequireReversible bool `yaml:"require_reversible"`

	// Rules configures the individual advisory rules. A nil rule is
	// enabled with its defaults.
	Rules SupervisorRulesConfig `yaml:"rules"`

	// Policy is the supervisor policy mode
	// (docs/supervisor-policy-design.md §4.9): who runs the meta-agent loop
	// over a dispatch. None (the empty string) and "none" are today's
	// behavior — no policy loop, no `supervise` tool, rules engine only.
	// "agent" registers the `supervise` tool so the DISPATCHING AGENT's own
	// turn cycle is the loop (design §4.7). "headless" is the host-driven
	// measurement loop (design §4.8, yaah-0nbb): a stateless one-shot decider
	// per tick, no conversation and no tools, requiring PolicyModel.
	//
	// The field is a string rather than a bool because the vocabulary is the
	// design's: a later mode must be addable without re-reading every
	// configuration, and an unknown value must be a visible error instead of
	// falling back to "off". Use PolicyMode/PolicyAgent/PolicyHeadless, not the
	// raw field.
	Policy string `yaml:"policy"`

	// PolicyInterval is the policy loop's tick cadence (design §4.9
	// `interval`, default 5s), as a Go duration string ("5s", "250ms"). It
	// paces the host loop in headless mode and the wait between ticks in agent
	// mode — the batch's own Interval — so one key sets the tempo both modes
	// are measured at. Read it through TickInterval, never raw: an
	// unparseable value is a load-time error, and the accessor is what supplies
	// the documented default.
	PolicyInterval string `yaml:"interval"`

	// PolicyTrigger selects when the loop spends a model call (design §4.4,
	// §4.9): TriggerAlways (the default — every tick with a live worker, the
	// paper's protocol) or TriggerOnChange (only when the ingest that ended the
	// previous tick saw a change). Read it through TriggerMode.
	PolicyTrigger string `yaml:"trigger"`

	// PolicyActions is the policy's action mask (design §4.9 `actions`): which
	// verbs a policy decision may use. ABSENT means all three (steer, redirect,
	// revert). An EXPLICIT empty list masks every action, which is how a
	// configuration turns the policy's interventions off without turning the
	// policy off — the loop still ticks and still records (and the rules engine
	// still routes through its arbiter), it just applies nothing. That
	// distinction is why the field is a slice rather than three bools: a nil
	// slice and an empty one must not mean the same thing (ActionMask reports
	// which one the configuration holds).
	PolicyActions []string `yaml:"actions"`

	// PolicyCooldownTicks is how many ticks after an intervention a scope
	// ignores further ones (design §4.9 `cooldown_ticks`, default 1). A pointer
	// because an EXPLICIT 0 means "no cooldown" while an absent key means the
	// documented default of one tick; the zero value of an int cannot say which
	// the operator meant. Read it through CooldownTicks.
	PolicyCooldownTicks *int `yaml:"cooldown_ticks"`

	// PolicyMaxInterventions caps the policy's applied interventions over a
	// batch's whole life (design §4.9 `max_interventions`); 0 — the default —
	// means unlimited. A rules stop is exempt in both directions (it never
	// blocks on the cap and never spends it): the cap governs the POLICY's
	// actions, and a stop is the rules engine's safety valve (design §4.6).
	PolicyMaxInterventions int `yaml:"max_interventions"`

	// PolicyPrompt names the meta-prompt version (design §4.9 `prompt`,
	// default v1). Only the locked v1 asset exists
	// (internal/prompts/supervision/meta_prompt_v1.md, byte-pinned by its own
	// drift test); an unknown value is a load-time error rather than a silent
	// fallback, because a measurement is only comparable to the paper's if the
	// prompt is the one it locked. Read it through PromptVersion.
	PolicyPrompt string `yaml:"prompt"`

	// PolicyModel is the model that runs the headless decider (design §4.9
	// `policy_model`): "provider/model" when the prefix names a configured
	// provider, else a bare model on the session's default provider — the same
	// spelling compaction_model uses. REQUIRED when Policy is headless, and a
	// missing one is a load-time validation error: the decider is one model
	// call per tick, so without a model the mode has nothing to decide with.
	PolicyModel string `yaml:"policy_model"`
}

// Supervisor policy defaults (docs/supervisor-policy-design.md §4.9). They
// mirror the batch's own defaults — supervision.DefaultBatchInterval and
// supervision.DefaultCooldownTicks, which the batch applies to a zero/negative
// config field — and the command layer pins the two against each other, so a
// drift between the documented default and the one a batch actually uses fails
// a test instead of a measurement.
const (
	// DefaultSupervisorInterval is `interval`'s default: the design's 5s tick.
	DefaultSupervisorInterval = 5 * time.Second
	// DefaultSupervisorCooldownTicks is `cooldown_ticks`'s default: a scope
	// acted on in tick N ignores a new action in tick N+1.
	DefaultSupervisorCooldownTicks = 1
)

// Supervisor policy vocabulary (docs/supervisor-policy-design.md §4.4, §4.9).
const (
	// SupervisorTriggerAlways asks the decider every tick with a live worker.
	SupervisorTriggerAlways = "always"
	// SupervisorTriggerOnChange asks it only when the previous tick's ingest
	// saw a change.
	SupervisorTriggerOnChange = "on-change"

	// SupervisorActionSteer, SupervisorActionRedirect and
	// SupervisorActionRevert are the policy's three verbs (design §4.5). The
	// rules engine's stop is NOT among them: a decider may not end a worker's
	// run (design D6).
	SupervisorActionSteer    = "steer"
	SupervisorActionRedirect = "redirect"
	SupervisorActionRevert   = "revert"

	// SupervisorPromptV1 is the locked v1 meta-prompt (the design's `prompt:
	// v1`, aliased to the asset's own name meta_prompt_v1 by PromptVersion).
	SupervisorPromptV1 = "v1"
)

// Supervisor policy modes (docs/supervisor-policy-design.md §4.9).
const (
	// SupervisorPolicyNone is the default: no policy loop. The advisory rules
	// engine and the `supervisor` tool's direct verbs are unaffected.
	SupervisorPolicyNone = "none"
	// SupervisorPolicyAgent makes the dispatching agent the meta-agent: it
	// reads the snapshot the `supervise` tool returns and answers with one
	// decision list per tick.
	SupervisorPolicyAgent = "agent"
	// SupervisorPolicyHeadless is the host-driven one-shot decider
	// (design §4.8), the measurement mode (yaah-0nbb): the host runs a tick
	// loop per supervised batch, each tick a stateless JSON-mode completion
	// over the rendered snapshot. It requires PolicyModel and registers no
	// `supervise` tool — the decider can call nothing (design §4.12).
	SupervisorPolicyHeadless = "headless"
)

// SupervisorRulesConfig holds the per-rule switches of the rules
// engine: the advisory rules (destructive warning, error rate, stuck
// detection) and the blocking shell guard (adoption plan B2).
type SupervisorRulesConfig struct {
	// DestructiveTool warns on destructive tool declarations (advisory
	// inject-only; the blocking guard is B2). Nil = enabled.
	DestructiveTool *SupervisorRuleSwitch `yaml:"destructive_tool"`

	// DestructiveGuard denies a sub-agent shell call that its allowlist
	// does not admit, before execution (adoption plan B2, decision D3:
	// default-deny for the shell — a shell can execute anything a pattern
	// matcher cannot see). The structured tools are the approved path for
	// changes; their review is the merge gate's job (Phase C), not this
	// net. Nil = enabled, with an empty allowlist (deny every shell call).
	DestructiveGuard *SupervisorShellGuardConfig `yaml:"destructive_guard"`

	// HighErrorRate injects guidance when a scope's recent tool error
	// rate crosses the threshold. Nil = enabled with defaults.
	HighErrorRate *SupervisorHighErrorRateConfig `yaml:"high_error_rate"`

	// StuckDetection injects guidance when a scope repeats the same tool
	// call in a row. Nil = enabled with defaults.
	StuckDetection *SupervisorStuckDetectionConfig `yaml:"stuck_detection"`
}

// SupervisorRuleSwitch enables or disables one rule. Nil Enabled means
// enabled — the same deviation-not-permission shape as the master
// switch.
type SupervisorRuleSwitch struct {
	Enabled *bool `yaml:"enabled"`
}

// SupervisorShellGuardConfig carries the blocking shell guard's switch
// and its global command allowlist.
type SupervisorShellGuardConfig struct {
	Enabled *bool `yaml:"enabled"`

	// AllowCommands is the global default allowlist: the shell commands a
	// sub-agent may run, as patterns matched in full against the raw
	// command string (`*` and `?` globs, `*` also crossing arguments —
	// `git status*` admits `git status -s`). Empty (the default) admits
	// nothing, which is the guard's default-deny: a shell can execute
	// anything a pattern matcher cannot see. A role's own allow list —
	// the config role entry's allow_commands, else the role file's —
	// replaces this list for that role rather than adding to it.
	AllowCommands []string `yaml:"allow_commands"`
}

// SupervisorHighErrorRateConfig carries the error-rate rule's switch
// and thresholds. Zero values mean the rule's defaults (threshold 0.5,
// window 10).
type SupervisorHighErrorRateConfig struct {
	Enabled   *bool   `yaml:"enabled"`
	Threshold float64 `yaml:"threshold"`
	Window    int     `yaml:"window"`
}

// SupervisorStuckDetectionConfig carries the stuck-detection rule's
// switch and threshold. Zero values mean the rule's defaults (repeat
// threshold 3).
type SupervisorStuckDetectionConfig struct {
	Enabled         *bool `yaml:"enabled"`
	RepeatThreshold int   `yaml:"repeat_threshold"`
}

// EngineEnabled reports whether the advisory rules engine runs at all:
// the master switch is off only when it was set to false explicitly.
func (s SupervisorConfig) EngineEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// PolicyMode normalizes the policy mode: the empty string (key unset) is
// SupervisorPolicyNone, and surrounding case/whitespace is ignored so
// " Agent " is the agent mode rather than an unknown value.
func (s SupervisorConfig) PolicyMode() string {
	switch mode := strings.ToLower(strings.TrimSpace(s.Policy)); mode {
	case "":
		return SupervisorPolicyNone
	default:
		return mode
	}
}

// PolicyAgent reports whether the dispatching agent runs the meta-agent loop
// itself — the mode that registers the `supervise` tool. Every other mode,
// including the default, leaves the tool unregistered, so an unconfigured
// session is byte-identical to today's.
func (s SupervisorConfig) PolicyAgent() bool {
	return s.PolicyMode() == SupervisorPolicyAgent
}

// PolicyHeadless reports whether the HOST runs the meta-agent loop
// (design §4.8): one stateless decider call per tick, no conversation, no
// tools, and no `supervise` tool registered. It is mutually exclusive with
// PolicyAgent by construction — the mode is one key, not two switches.
func (s SupervisorConfig) PolicyHeadless() bool {
	return s.PolicyMode() == SupervisorPolicyHeadless
}

// TickInterval resolves the policy loop's cadence: the configured duration, or
// DefaultSupervisorInterval when the key is absent. An unparseable or
// non-positive value cannot reach here — validation refuses it at load — but a
// value that did would fall back to the default rather than to a zero interval,
// because a zero interval is a busy loop, not a slower one.
func (s SupervisorConfig) TickInterval() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s.PolicyInterval)); err == nil && d > 0 {
		return d
	}
	return DefaultSupervisorInterval
}

// TriggerMode normalizes the trigger: absent is TriggerAlways, and case and
// surrounding whitespace are ignored so " On-Change " is the on-change trigger
// rather than an unknown value. An unknown value is refused at load.
func (s SupervisorConfig) TriggerMode() string {
	switch t := strings.ToLower(strings.TrimSpace(s.PolicyTrigger)); t {
	case "":
		return SupervisorTriggerAlways
	default:
		return t
	}
}

// ActionMask resolves the policy's action mask and reports whether the
// configuration named it EXPLICITLY.
//
// nil means the key is absent, which is all three actions; a non-nil EMPTY
// slice means the operator wrote `actions: []`, which masks every action (see
// the field's doc). The second return value is what lets a caller state the
// difference to an operator instead of reporting both as "no actions
// configured".
func (s SupervisorConfig) ActionMask() ([]string, bool) {
	if s.PolicyActions == nil {
		return []string{SupervisorActionSteer, SupervisorActionRedirect, SupervisorActionRevert}, false
	}
	out := make([]string, 0, len(s.PolicyActions))
	seen := make(map[string]bool, len(s.PolicyActions))
	for _, a := range s.PolicyActions {
		name := strings.ToLower(strings.TrimSpace(a))
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, true
}

// CooldownTicks resolves the cooldown: the configured value, or the documented
// default of one tick when the key is absent. An explicit 0 is preserved — it
// means "no cooldown", which is a configuration, not a missing one.
func (s SupervisorConfig) CooldownTicks() int {
	if s.PolicyCooldownTicks == nil {
		return DefaultSupervisorCooldownTicks
	}
	return *s.PolicyCooldownTicks
}

// MaxInterventions resolves the intervention cap; 0 means unlimited, which is
// also the default, so the raw field is already total.
func (s SupervisorConfig) MaxInterventions() int {
	return s.PolicyMaxInterventions
}

// PromptVersion normalizes the meta-prompt version: absent is v1, and the
// asset's own name (meta_prompt_v1, the token internal/prompts/supervision
// documents) is accepted as an alias so a configuration written either way
// names the one shipped prompt. Anything else is refused at load.
func (s SupervisorConfig) PromptVersion() string {
	switch v := strings.ToLower(strings.TrimSpace(s.PolicyPrompt)); v {
	case "", SupervisorPromptV1, "meta_prompt_v1":
		return SupervisorPromptV1
	default:
		return v
	}
}

// PolicyModelName returns the configured decider model, trimmed. Empty means
// unconfigured, which is only legal outside headless mode (validation).
func (s SupervisorConfig) PolicyModelName() string {
	return strings.TrimSpace(s.PolicyModel)
}

// DispatchSupervised reports whether this session's DISPATCHES run supervised
// (design §4.2 decision D5, yaah-ugtl): only an EXPLICIT enabled: true arms
// the supervised dispatch path. The rules engine's default-on (EngineEnabled)
// deliberately does NOT arm it, and until the `supervisor.policy` key lands
// (yaah-0nbb) no other signal exists: reading the default as "supervise every
// dispatch" would upgrade every shared-lane dispatch and make a failed pre-run
// capture fatal for sessions that never asked for supervision — against D5,
// whose whole claim is that the default is today's behavior.
//
// Like the other resolutions on this type, this is the single place the
// decision is derived: cmd wiring calls it instead of re-deriving the pointer
// test (review M-r5).
func (s SupervisorConfig) DispatchSupervised() bool {
	return s.Enabled != nil && *s.Enabled
}

// DestructiveRuleEnabled reports the advisory destructive rule's
// switch, defaulting to enabled when unconfigured.
func (s SupervisorConfig) DestructiveRuleEnabled() bool {
	return s.Rules.DestructiveTool == nil || s.Rules.DestructiveTool.Enabled == nil || *s.Rules.DestructiveTool.Enabled
}

// DestructiveGuardEnabled reports the blocking shell guard's switch
// (adoption plan B2), defaulting to enabled when unconfigured.
func (s SupervisorConfig) DestructiveGuardEnabled() bool {
	return s.Rules.DestructiveGuard == nil || s.Rules.DestructiveGuard.Enabled == nil || *s.Rules.DestructiveGuard.Enabled
}

// DestructiveGuardAllow returns the global default shell-command
// allowlist for the blocking guard (supervisor.rules.destructive_guard.
// allow_commands). Nil when unconfigured — the guard then admits no
// shell call at all. A dispatching role's own allowlist replaces this
// list (see agents.subagent.roles.<role>.allow_commands).
func (s SupervisorConfig) DestructiveGuardAllow() []string {
	if s.Rules.DestructiveGuard == nil {
		return nil
	}
	return s.Rules.DestructiveGuard.AllowCommands
}

// HighErrorRateEnabled reports the error-rate rule's switch, defaulting
// to enabled when unconfigured.
func (s SupervisorConfig) HighErrorRateEnabled() bool {
	return s.Rules.HighErrorRate == nil || s.Rules.HighErrorRate.Enabled == nil || *s.Rules.HighErrorRate.Enabled
}

// StuckDetectionEnabled reports the stuck-detection rule's switch,
// defaulting to enabled when unconfigured.
func (s SupervisorConfig) StuckDetectionEnabled() bool {
	return s.Rules.StuckDetection == nil || s.Rules.StuckDetection.Enabled == nil || *s.Rules.StuckDetection.Enabled
}

// ErrorRateThreshold reports the error-rate rule's configured
// threshold, zero (the kernel rule's own default) when unconfigured.
func (s SupervisorConfig) ErrorRateThreshold() float64 {
	if s.Rules.HighErrorRate == nil {
		return 0
	}
	return s.Rules.HighErrorRate.Threshold
}

// ErrorRateWindow reports the error-rate rule's configured window,
// zero (the kernel rule's own default) when unconfigured.
func (s SupervisorConfig) ErrorRateWindow() int {
	if s.Rules.HighErrorRate == nil {
		return 0
	}
	return s.Rules.HighErrorRate.Window
}

// StuckRepeatThreshold reports the stuck-detection rule's configured
// threshold, zero (the kernel rule's own default) when unconfigured.
func (s SupervisorConfig) StuckRepeatThreshold() int {
	if s.Rules.StuckDetection == nil {
		return 0
	}
	return s.Rules.StuckDetection.RepeatThreshold
}

// TracePath returns the Goatherd trace store file path. Tracing is
// integral, so there is no empty/off result: an unset goatherd_trace_path
// resolves to <yaah home>/trace.db, honoring $YAAH_HOME.
func (d Defaults) TracePath() string {
	if p := strings.TrimSpace(d.GoatherdTracePath); p != "" {
		return p
	}
	return filepath.Join(HomeDir(), "trace.db")
}

// TraceStoreConfig resolves where traces live — backend and DSN — into the
// seam the tracestore package opens. Resolution order, per field: the
// goatherd_trace stanza wins; the deprecated flat goatherd_trace_path
// remains the SQLite DSN when the stanza carries none; the SQLite default
// is the yaah-home trace file. The backend is normalized (empty is the
// SQLite default) so callers can branch on IsSQLite without re-validating.
// A PostgreSQL backend without a DSN surfaces as an error at Open, not as
// a silently wrong default.
func (d Defaults) TraceStoreConfig() tracestore.Config {
	backend := strings.TrimSpace(d.GoatherdTrace.Backend)
	if backend == "" {
		backend = tracestore.BackendSQLite
	}
	dsn := strings.TrimSpace(d.GoatherdTrace.DSN)
	if dsn == "" && backend == tracestore.BackendSQLite {
		dsn = d.TracePath()
	}
	return tracestore.Config{Backend: backend, DSN: dsn}
}

// HaltRootEnabled resolves the root-halt opt-in. The stanza's explicit
// value wins in both directions; unset falls back to the A3 legacy
// flat key, so existing configurations keep gating the same switch
// and the stanza remains the one home going forward.
func (d Defaults) HaltRootEnabled() bool {
	if d.Supervisor.HaltRoot != nil {
		return *d.Supervisor.HaltRoot
	}
	return d.SupervisorHaltRoot
}

// validateSupervisorPolicy checks the policy mode AND its tuning keys
// (design §4.9). The three modes are supported; "headless" additionally
// requires a decider model, because a host loop with no model has nothing to
// decide with.
//
// Every policy key is checked here regardless of the mode, unlike the rule
// thresholds below (which are gated on the rule being enabled): a typo in
// `trigger`, `actions`, `interval`, `prompt` or `policy_model` is never inert
// — it silently changes what a policy loop does, or makes it spend model
// calls doing the wrong thing — and all of these keys are new, so no existing
// configuration can be broken by refusing one.
func validateSupervisorPolicy(d Defaults) []string {
	sup := d.Supervisor
	var errs []string
	switch mode := sup.PolicyMode(); mode {
	case SupervisorPolicyNone, SupervisorPolicyAgent:
	case SupervisorPolicyHeadless:
		if sup.PolicyModelName() == "" {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.policy_model is required when agents.default.supervisor.policy is %q: the headless decider is one model call per tick, so without a model it has nothing to decide with — set policy_model (\"provider/model\", or a bare model on the session's default provider), or use policy: %q or %q",
				SupervisorPolicyHeadless, SupervisorPolicyAgent, SupervisorPolicyNone))
		}
	default:
		errs = append(errs, fmt.Sprintf(
			"agents.default.supervisor.policy %q is not a policy mode: use %q (the default, no policy loop), %q (the dispatching agent runs the loop), or %q (the host runs a one-shot decider per tick)",
			sup.Policy, SupervisorPolicyNone, SupervisorPolicyAgent, SupervisorPolicyHeadless))
	}
	return append(errs, validateSupervisorPolicyKeys(sup)...)
}

// validateSupervisorPolicyKeys checks the tuning keys themselves, in the order
// the design lists them. Each check names the key and both ways out.
func validateSupervisorPolicyKeys(sup SupervisorConfig) []string {
	var errs []string
	if raw := strings.TrimSpace(sup.PolicyInterval); raw != "" {
		if d, err := time.ParseDuration(raw); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.interval %q must be a positive Go duration (\"5s\", \"250ms\") or absent for the %s default", raw, DefaultSupervisorInterval))
		}
	}
	if raw := strings.TrimSpace(sup.PolicyTrigger); raw != "" {
		switch sup.TriggerMode() {
		case SupervisorTriggerAlways, SupervisorTriggerOnChange:
		default:
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.trigger %q must be %q (the default: a decider call every tick with a live worker) or %q (only when the previous tick saw a change)",
				raw, SupervisorTriggerAlways, SupervisorTriggerOnChange))
		}
	}
	for _, a := range sup.PolicyActions {
		switch strings.ToLower(strings.TrimSpace(a)) {
		case SupervisorActionSteer, SupervisorActionRedirect, SupervisorActionRevert:
		default:
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.actions entry %q must be one of %q, %q or %q (omit the key for all three; an explicit empty list allows none)",
				a, SupervisorActionSteer, SupervisorActionRedirect, SupervisorActionRevert))
		}
	}
	if sup.PolicyCooldownTicks != nil && *sup.PolicyCooldownTicks < 0 {
		errs = append(errs, fmt.Sprintf(
			"agents.default.supervisor.cooldown_ticks %d must be 0 (no cooldown) or positive (omit the key for the %d-tick default)",
			*sup.PolicyCooldownTicks, DefaultSupervisorCooldownTicks))
	}
	if sup.PolicyMaxInterventions < 0 {
		errs = append(errs, fmt.Sprintf(
			"agents.default.supervisor.max_interventions %d must be 0 (unlimited) or positive", sup.PolicyMaxInterventions))
	}
	if raw := strings.TrimSpace(sup.PolicyPrompt); raw != "" {
		if v := sup.PromptVersion(); v != SupervisorPromptV1 {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.prompt %q is not a meta-prompt version: the only locked asset is %q (internal/prompts/supervision/meta_prompt_v1.md, aliased %q), whose bytes a measurement is comparable to the paper's only if they are the ones it sends",
				raw, SupervisorPromptV1, "meta_prompt_v1"))
		}
	}
	return errs
}

// validateSupervisor checks the supervisor stanza's numeric settings.
// Zeros are the documented defaults; an explicit value must be usable
// by the rule it tunes — an error-rate threshold above 1 would make an
// enabled rule impossible to trigger, and negatives are neither
// defaults nor thresholds.
//
// The check follows the stanza's own contract: it applies only to rules
// that are actually enabled — values the engine never reads (engine
// off, rule off) cannot reject an otherwise valid config.
func validateSupervisor(d Defaults) []string {
	// The policy mode is checked BEFORE the rules-engine switch: the switch
	// governs the advisory RULES, while the policy decides whether the
	// session's tool surface gains a `supervise` tool at all. A typo there is
	// never inert — it silently leaves a session unsupervised — so it is
	// refused even with the engine off.
	errs := validateSupervisorPolicy(d)
	if !d.Supervisor.EngineEnabled() {
		return errs
	}
	sup := d.Supervisor
	if sup.HighErrorRateEnabled() {
		if t := sup.ErrorRateThreshold(); t < 0 || t > 1 {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.rules.high_error_rate.threshold %g must be 0 (default) or within (0, 1]", t))
		}
		if w := sup.ErrorRateWindow(); w < 0 {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.rules.high_error_rate.window %d must be 0 (default) or positive", w))
		}
	}
	if sup.StuckDetectionEnabled() {
		if r := sup.StuckRepeatThreshold(); r < 0 {
			errs = append(errs, fmt.Sprintf(
				"agents.default.supervisor.rules.stuck_detection.repeat_threshold %d must be 0 (default) or positive", r))
		}
	}
	return errs
}
