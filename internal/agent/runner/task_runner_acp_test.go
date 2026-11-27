package runner

import (
	"context"
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/tools/substrate"
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
	var seenParams tools.SubAgentParams
	opts := taskRunnerOpts{
		outputLimit: 7,
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner {
			if backend != "acp" || agent != "vibe" {
				t.Fatalf("resolver got %q/%q", backend, agent)
			}
			return func(_ context.Context, prompt string, params tools.SubAgentParams) (string, error) {
				called = true
				seenParams = params
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
	if seenParams.OutputLimit != 7 {
		t.Fatalf("ACP runner saw output limit %d, want resolved cap 7", seenParams.OutputLimit)
	}
}

func TestMakeTaskRunnerACPUsesWorkspaceWorkdir(t *testing.T) {
	var seen tools.SubAgentParams
	opts := taskRunnerOpts{
		pathValidator: &substrate.PathValidator{WorkDir: "/session/worktree"},
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner {
			return func(_ context.Context, _ string, params tools.SubAgentParams) (string, error) {
				seen = params
				return "ok", nil
			}
		},
	}
	run := makeTaskRunner(opts, 1)
	if _, err := run(context.Background(), "hi", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen.Workdir != "/session/worktree" {
		t.Fatalf("ACP runner workdir = %q, want the session workspace", seen.Workdir)
	}
}

type workdirWorkspace struct {
	substrate.Workspace
	dir string
}

func (w workdirWorkspace) WorkDir() string         { return w.dir }
func (w workdirWorkspace) Local() bool             { return true }
func (w workdirWorkspace) Shell() (string, string) { return "sh", "-c" }

func TestMakeTaskRunnerACPPrefersWorkspaceWorkdir(t *testing.T) {
	var seen tools.SubAgentParams
	opts := taskRunnerOpts{
		workspace:     workdirWorkspace{dir: "/sandbox/workspace"},
		pathValidator: &substrate.PathValidator{WorkDir: "/host/worktree"},
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner {
			return func(_ context.Context, _ string, params tools.SubAgentParams) (string, error) {
				seen = params
				return "ok", nil
			}
		},
	}
	run := makeTaskRunner(opts, 1)
	if _, err := run(context.Background(), "hi", tools.SubAgentParams{Role: "developer"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen.Workdir != "/sandbox/workspace" {
		t.Fatalf("ACP runner workdir = %q, want the sandbox workspace", seen.Workdir)
	}
}

func TestMakeTaskRunnerACPRefusesProposalLane(t *testing.T) {
	opts := taskRunnerOpts{
		subCfg: config.SubAgentConfig{Roles: map[string]config.RoleConfig{
			"developer": {Backend: "acp", ACPAgent: "vibe"},
		}},
		resolveTaskRunner: func(backend, agent string) tools.TaskRunner {
			return func(context.Context, string, tools.SubAgentParams) (string, error) { return "ok", nil }
		},
	}
	run := makeTaskRunner(opts, 1)
	_, err := run(context.Background(), "hi", tools.SubAgentParams{Role: "developer", Lane: "retained"})
	if err == nil {
		t.Fatal("proposal lane must be refused for an ACP backend")
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
