package runner

// supervise_boundary_test.go pins the boundary that keeps the meta-agent loop
// surface out of every sub-agent (yaah-fy28, review MAJOR 3): the `supervise`
// tool binds ONE batch to the calling loop's scope, so a sub-agent that could
// call it would join — or be refused by, or worse, be confused with — the
// root's batch and steer its SIBLINGS. Sub-agent registries are built from the
// role profile's tool list through tools.NewLeafTool, and supervise (like
// supervisor and proposal) is a runtime-wired tool, not a leaf tool: a role
// file naming it gets nothing registered.

import (
	"testing"

	"github.com/goatherdai/yaah/internal/agent/subagent"
	"github.com/goatherdai/yaah/internal/config"
)

// TestBuildSubAgentRegistry_NeverExposesOrchestratorTools pins the guarantee
// for the dispatch lane: a role file that lists the orchestrator-only control
// tools gets none of them.
func TestBuildSubAgentRegistry_NeverExposesOrchestratorTools(t *testing.T) {
	workdir := t.TempDir()
	reg, _, err := buildSubAgentRegistry(
		taskRunnerOpts{subCfg: config.SubAgentConfig{Containment: "off"}},
		subagent.RoleProfile{Tools: []string{"supervise", "supervisor", "proposal", "read"}},
		"", 0, workdir)
	if err != nil {
		t.Fatalf("buildSubAgentRegistry: %v", err)
	}
	for _, name := range []string{"supervise", "supervisor", "proposal"} {
		if tool := reg.Get(name); tool != nil {
			t.Errorf("%s is registered on a sub-agent: the meta-agent loop and the settlement surface are the orchestrator's alone", name)
		}
	}
	// The rest of the profile still lands: the omission is the tool list's
	// business, not a dispatch failure.
	if reg.Get("read") == nil {
		t.Error("read is not registered, so the profile was dropped wholesale")
	}
}
