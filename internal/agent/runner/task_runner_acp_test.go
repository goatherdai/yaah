package runner

import (
	"context"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/tools"
)

func TestResolveDispatchRouteACPBackend(t *testing.T) {
	opts := taskRunnerOpts{
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
	}
	route := resolveDispatchRoute(opts, "developer", tools.SubAgentParams{}, subagent.RoleProfile{})
	if route.backend != "acp" || route.agent != "vibe" {
		t.Fatalf("route = %+v, want backend acp / agent vibe", route)
	}
}

func TestMakeTaskRunnerACPBackend(t *testing.T) {
	called := false
	opts := taskRunnerOpts{
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner {
			if backend != "acp" || agent != "vibe" {
				t.Fatalf("resolver got %q/%q", backend, agent)
			}
			return func(_ context.Context, prompt string, _ tools.SubAgentParams) (string, error) {
				called = true
				return "ACP:" + prompt, nil
			}
		},
	}
	run := makeTaskRunner(opts, 1)
	got, err := run(context.Background(), "hi", tools.SubAgentParams{Role: "developer"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !called || got != "ACP:hi" {
		t.Fatalf("called=%v text=%q", called, got)
	}
}

func TestMakeTaskRunnerMissingACPAgentFailsClosed(t *testing.T) {
	opts := taskRunnerOpts{
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "missing"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner { return nil },
	}
	run := makeTaskRunner(opts, 1)
	if _, err := run(context.Background(), "hi", tools.SubAgentParams{Role: "developer"}); err == nil {
		t.Fatal("unknown ACP agent should fail closed")
	}
}
