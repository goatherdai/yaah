package config

// supervisor_policy_test.go pins the supervisor policy gate's shape (yaah-fy28
// + yaah-0nbb, design §4.9): the empty string and "none" are no policy loop,
// "agent" registers the supervise tool, and "headless" requires a decider
// model — a headless stanza without policy_model fails at LOAD rather than
// running a loop that can never decide. TestSupervisorPolicyKeys pins the
// tuning keys' validation the same way: one fully valid headless stanza
// accepted, and every bad interval / trigger / action / cooldown / cap /
// prompt refused with an error naming the key (PR #417 review round 2,
// finding 3).

import (
	"strings"
	"testing"
)

func TestSupervisorPolicyMode(t *testing.T) {
	var s SupervisorConfig
	if got := s.PolicyMode(); got != SupervisorPolicyNone {
		t.Errorf("unset policy mode = %q, want %q", got, SupervisorPolicyNone)
	}
	if s.PolicyAgent() {
		t.Error("unset policy must not select the agent mode")
	}
	s.Policy = "  Agent "
	if !s.PolicyAgent() {
		t.Errorf("policy %q must normalize to the agent mode", s.Policy)
	}
	s.Policy = SupervisorPolicyNone
	if s.PolicyAgent() {
		t.Error("policy none must not select the agent mode")
	}
}

func TestSupervisorPolicyValidation(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		wantErr string
	}{
		{name: "none passes", policy: "none"},
		{name: "agent passes", policy: "agent"},
		{name: "absent passes"},
		{
			name:    "headless without a policy model",
			policy:  "headless",
			wantErr: "policy_model",
		},
		{
			name:    "an unknown mode is refused",
			policy:  "sometimes",
			wantErr: "not a policy mode",
		},
		{
			// The policy decides the tool surface, so a typo is never inert:
			// the rules-engine switch must not swallow it.
			name:    "engine off does not make the policy inert",
			policy:  "sometimes",
			wantErr: "not a policy mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stanza := "agents:\n  default:\n    supervisor:\n      policy: " + tc.policy + "\n"
			if tc.name == "engine off does not make the policy inert" {
				stanza = "agents:\n  default:\n    supervisor:\n      enabled: false\n      policy: " + tc.policy + "\n"
			}
			writeConfig(t, stanza)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			verr := Validate(cfg)
			if tc.wantErr == "" {
				if verr != nil {
					t.Fatalf("Validate = %v, want no errors", verr)
				}
				return
			}
			if verr == nil || !strings.Contains(verr.Error(), tc.wantErr) {
				t.Errorf("Validate = %v, want %q named", verr, tc.wantErr)
			}
		})
	}
}

// TestSupervisorPolicyAgentParses pins the yaml key itself.
func TestSupervisorPolicyAgentParses(t *testing.T) {
	writeConfig(t, "agents:\n  default:\n    supervisor:\n      policy: agent\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.Default.Supervisor.PolicyAgent() {
		t.Errorf("supervisor.policy: agent parsed as %q", cfg.Agent.Default.Supervisor.Policy)
	}
}

// TestSupervisorPolicyKeys pins the tuning-key validation (PR #417 review
// round 2, finding 3): a fully valid headless stanza — one that a measurement
// run is actually launched with — must pass Validate, and every key the design
// names must refuse a bad value with an error naming THAT key. Without this
// table nothing in the suite exercises validateSupervisorPolicyKeys: a
// regression that silently accepts `trigger: on-mondays` or `interval: soon`
// would arm a loop running at the wrong cadence, and a regression that
// refuses a VALID stanza would make headless mode unlaunchable.
func TestSupervisorPolicyKeys(t *testing.T) {
	// The valid baseline: every policy key set to a legal, non-default value.
	// Cases mutate ONE key at a time from this stanza.
	valid := []string{
		"policy: headless",
		"policy_model: deepseek/test-decider",
		"interval: 250ms",
		"trigger: on-change",
		"actions: [steer, revert]",
		"cooldown_ticks: 0",
		"max_interventions: 3",
		"prompt: v1",
	}
	cases := []struct {
		name    string
		lines   []string
		wantErr string
	}{
		{name: "a fully valid headless stanza passes", lines: valid},
		{name: "the prompt alias passes", lines: replace(valid, "prompt: v1", "prompt: meta_prompt_v1")},
		{
			name:    "an unparseable interval is refused",
			lines:   replace(valid, "interval: 250ms", "interval: soon"),
			wantErr: "interval",
		},
		{
			name:    "a non-positive interval is refused",
			lines:   replace(valid, "interval: 250ms", "interval: -5s"),
			wantErr: "interval",
		},
		{
			name:    "an unknown trigger is refused",
			lines:   replace(valid, "trigger: on-change", "trigger: on-mondays"),
			wantErr: "trigger",
		},
		{
			name:    "an unknown action is refused",
			lines:   replace(valid, "actions: [steer, revert]", "actions: [steer, nudge]"),
			wantErr: "actions",
		},
		{
			name:    "a negative cooldown is refused",
			lines:   replace(valid, "cooldown_ticks: 0", "cooldown_ticks: -1"),
			wantErr: "cooldown_ticks",
		},
		{
			name:    "a negative intervention cap is refused",
			lines:   replace(valid, "max_interventions: 3", "max_interventions: -2"),
			wantErr: "max_interventions",
		},
		{
			name:    "an unknown prompt version is refused",
			lines:   replace(valid, "prompt: v1", "prompt: v2"),
			wantErr: "prompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, "agents:\n  default:\n    supervisor:\n      "+strings.Join(tc.lines, "\n      ")+"\n")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			verr := Validate(cfg)
			if tc.wantErr == "" {
				if verr != nil {
					t.Fatalf("Validate = %v, want no errors for a legal stanza", verr)
				}
				return
			}
			if verr == nil || !strings.Contains(verr.Error(), tc.wantErr) {
				t.Errorf("Validate = %v, want an error naming %q", verr, tc.wantErr)
			}
		})
	}
}

// replace returns lines with the first occurrence of old swapped for new.
func replace(lines []string, old, new string) []string {
	out := append([]string(nil), lines...)
	for i, l := range out {
		if l == old {
			out[i] = new
			return out
		}
	}
	panic("replace: line not found: " + old)
}
