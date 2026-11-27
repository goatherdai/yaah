package yaah

import (
	"context"
	"os"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/workflow/exec"
	"github.com/goatherdai/yaah/internal/workflow/host"
	"github.com/goatherdai/yaah/internal/workflow/ir"
	"github.com/goatherdai/yaah/internal/workflow/spec"
)

const acpHelperEnv = "YAAH_ACP_HELPER_AGENT"

// helperAgent is a minimal ACP agent used by TestHelperProcessACP. It runs on
// the test binary's stdin/stdout and answers one prompt with "PONG".
type helperAgent struct {
	conn *acp.AgentSideConnection
}

func (a *helperAgent) SetAgentConnection(c *acp.AgentSideConnection) { a.conn = c }
func (a *helperAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: "test-helper", Version: "0"},
	}, nil
}
func (a *helperAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *helperAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *helperAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *helperAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *helperAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}
func (a *helperAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *helperAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *helperAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *helperAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *helperAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId("sess-helper")}, nil
}
func (a *helperAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: p.SessionId,
		Update:    acp.UpdateAgentMessageText("PONG"),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// TestHelperProcessACP is launched as a subprocess by the wiring test. It is a
// no-op in a normal test run.
func TestHelperProcessACP(t *testing.T) {
	if os.Getenv(acpHelperEnv) != "1" {
		return
	}
	agent := &helperAgent{}
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.SetAgentConnection(conn)
	<-conn.Done()
	os.Exit(0)
}

func TestACPTaskRunnerWiring(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.SubAgent.ACPAgents = map[string]config.ACPAgentConfig{
		"helper": {
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestHelperProcessACP$"},
			Env:     []string{acpHelperEnv + "=1"},
			Cwd:     t.TempDir(),
		},
	}
	cfg.Agent.SubAgent.Roles = map[string]config.RoleConfig{
		"developer": {Backend: "acp", ACPAgent: "helper"},
	}
	resolver := buildACPTaskRunnerResolver(cfg)
	if resolver("loop", "") != nil {
		t.Fatal("loop backend must not resolve through the ACP registry")
	}
	runner := resolver("acp", "helper")
	if runner == nil {
		t.Fatal("configured ACP agent did not resolve")
	}
	if resolver("acp", "missing") != nil {
		t.Fatal("unknown ACP agent must fail closed")
	}

	y := "apiVersion: yaah.workflow/v1\nkind: Workflow\nmetadata:\n  name: acp-e2e\nspec:\n" + `  steps:
    - id: work
      kind: delegate
      role: developer
      prompt: say PONG
`
	doc, err := spec.ParseYAML([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g, err := ir.Resolve(doc)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	res, err := host.Execute(context.Background(), host.Options{Graph: g, Runner: runner})
	if err != nil {
		t.Fatalf("workflow execute: %v", err)
	}
	if res.Run.Status != exec.RunSucceeded {
		t.Fatalf("status = %s, reason = %q", res.Run.Status, res.Run.Reason)
	}
	for _, n := range res.Run.Nodes {
		if n.Path == "steps.work" && strings.Contains(n.Result, "PONG") {
			return
		}
	}
	t.Fatal("no delegate node returned PONG")
}
