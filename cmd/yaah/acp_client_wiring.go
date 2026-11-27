package yaah

import (
	"github.com/goatherdai/yaah/internal/acpclient"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/jobs"
)

// buildACPTaskRunnerResolver turns the configured ACP agent manifests into the
// backend seam injected into both the delegate tool and the workflow runner.
// Non-ACP backends resolve to nil so the in-process loop remains the default.
func buildACPTaskRunnerResolver(cfg *config.Config) func(backend, agent string) jobs.TaskRunner {
	manifests := make(map[string]acpclient.Manifest, len(cfg.Agent.SubAgent.ACPAgents))
	for name, entry := range cfg.Agent.SubAgent.ACPAgents {
		manifests[name] = acpclient.Manifest{
			Name:    name,
			Command: entry.Command,
			Args:    entry.Args,
			Env:     entry.Env,
			Cwd:     entry.Cwd,
		}
	}
	registry := acpclient.NewRegistry(manifests)
	return func(backend, agent string) jobs.TaskRunner {
		if backend != "acp" {
			return nil
		}
		return registry.Resolve(agent)
	}
}
