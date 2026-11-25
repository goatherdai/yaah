package config

// supervisor_policy_test.go pins the supervisor policy gate's shape (yaah-fy28,
// design §4.9): the empty string and "none" are today's behavior, "agent" is the
// mode that registers the supervise tool, and "headless" is named but refused —
// it lands in yaah-0nbb, and a configuration written against the design must
// fail at load instead of running with no decider.

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
			name:    "headless names its bead",
			policy:  "headless",
			wantErr: "yaah-0nbb",
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
