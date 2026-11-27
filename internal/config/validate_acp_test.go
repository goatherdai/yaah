package config

import (
	"strings"
	"testing"
)

func TestValidateACPAgents(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.ACPAgents = map[string]ACPAgentConfig{
		"vibe": {Command: "vibe-acp", Args: []string{}},
	}
	cfg.Agent.SubAgent.Roles = map[string]RoleConfig{
		"developer": {Backend: "acp", ACPAgent: "vibe"},
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid ACP backend should pass: %v", err)
	}
}

func TestValidateACPRejectsUnknownBackend(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.Roles = map[string]RoleConfig{
		"developer": {Backend: "magic"},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "roles.developer.backend") {
		t.Fatalf("unknown backend should be rejected, got %v", err)
	}
}

func TestValidateACPRequiresAgent(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.Roles = map[string]RoleConfig{
		"developer": {Backend: "acp"},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "requires acp_agent") {
		t.Fatalf("missing acp_agent should be rejected, got %v", err)
	}
}

func TestValidateACPRequiresDeclaredAgent(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.Roles = map[string]RoleConfig{
		"developer": {Backend: "acp", ACPAgent: "missing"},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared acp_agent should be rejected, got %v", err)
	}
}

func TestValidateACPRejectsMalformedEnv(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.ACPAgents = map[string]ACPAgentConfig{
		"vibe": {Command: "vibe-acp", Env: []string{"BROKEN"}},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "must be KEY=VALUE") {
		t.Fatalf("malformed env should be rejected at startup, got %v", err)
	}
}

func TestValidateACPRejectsIsolatedWorld(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.SubAgent.ACPAgents = map[string]ACPAgentConfig{
		"vibe": {Command: "vibe-acp"},
	}
	cfg.Agent.SubAgent.Roles = map[string]RoleConfig{
		"developer": {Backend: "acp", ACPAgent: "vibe", World: "worktree"},
	}
	err := Validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot use world") {
		t.Fatalf("isolated world with ACP backend should be rejected, got %v", err)
	}
}
