package acpclient

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
)

// fakeAgent is an in-process ACP agent used to test the client without a real
// external binary. It implements the stable acp.Agent surface.
type fakeAgent struct {
	conn               *acp.AgentSideConnection
	reply              string
	askPermission      bool
	permissionGranted  bool
	permissionSelected string
	stopReason         acp.StopReason
	block              bool
	systemPrompt       bool
	seenPrompt         string
	seenSystem         *acp.SystemPrompt
	cachedPrompt       int
	totalTokens        int
	alwaysFirst        bool
}

func permissionOptions(alwaysFirst bool) []acp.PermissionOption {
	once := acp.PermissionOption{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")}
	always := acp.PermissionOption{Kind: acp.PermissionOptionKindAllowAlways, Name: "Always", OptionId: acp.PermissionOptionId("always")}
	reject := acp.PermissionOption{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")}
	if alwaysFirst {
		return []acp.PermissionOption{always, once, reject}
	}
	return []acp.PermissionOption{once, reject, always}
}

func (a *fakeAgent) SetAgentConnection(c *acp.AgentSideConnection) { a.conn = c }

func (a *fakeAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	caps := acp.AgentCapabilities{}
	if a.systemPrompt {
		caps.SessionCapabilities.SystemPrompt = &acp.SessionSystemPromptCapabilities{}
	}
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentInfo:         &acp.Implementation{Name: "fake", Version: "0"},
		AgentCapabilities: caps,
	}, nil
}

func (a *fakeAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *fakeAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}
func (a *fakeAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *fakeAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, nil
}
func (a *fakeAgent) DeleteSession(context.Context, acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	return acp.DeleteSessionResponse{}, nil
}
func (a *fakeAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, nil
}
func (a *fakeAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, nil
}
func (a *fakeAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}
func (a *fakeAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}
func (a *fakeAgent) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	a.seenSystem = p.SystemPrompt
	return acp.NewSessionResponse{SessionId: acp.SessionId("sess-fake")}, nil
}

func (a *fakeAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	a.seenPrompt = p.Prompt[0].Text.Text
	if a.block {
		<-ctx.Done()
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, ctx.Err()
	}
	if a.askPermission {
		resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: p.SessionId,
			ToolCall:  acp.ToolCallUpdate{},
			Options:   permissionOptions(a.alwaysFirst),
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if resp.Outcome.Selected == nil {
			return acp.PromptResponse{}, acp.NewInternalError(map[string]any{"error": "permission cancelled"})
		}
		a.permissionSelected = string(resp.Outcome.Selected.OptionId)
		a.permissionGranted = a.permissionSelected == "allow"
	}
	usageMeta := map[string]any{"promptTokens": 10, "completionTokens": 3, "cachedTokens": 2}
	if a.cachedPrompt > 0 {
		usageMeta["cachedPromptTokens"] = a.cachedPrompt
	}
	if a.totalTokens > 0 {
		usageMeta["totalTokens"] = a.totalTokens
	}
	meta, err := acp.NewMeta(usageMeta)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	updates := []acp.SessionUpdate{
		{UsageUpdate: &acp.SessionUsageUpdate{Meta: meta, Size: 1000, Used: 13}},
		acp.UpdateAgentMessageText(a.reply),
	}
	for _, u := range updates {
		if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: p.SessionId, Update: u}); err != nil {
			return acp.PromptResponse{}, err
		}
	}
	reason := a.stopReason
	if reason == "" {
		reason = acp.StopReasonEndTurn
	}
	return acp.PromptResponse{StopReason: reason}, nil
}

func wire(t *testing.T, agent *fakeAgent, c acp.Client) *connection {
	t.Helper()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	asc := acp.NewAgentSideConnection(agent, a2cW, c2aR)
	asc.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	agent.SetAgentConnection(asc)
	conn := acp.NewClientSideConnection(c, c2aW, a2cR)
	conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})
	return &connection{conn: conn, close: func() {}, stderr: func() string { return "" }}
}

func TestManifestValidate(t *testing.T) {
	if err := (Manifest{}).Validate(); err == nil {
		t.Fatal("empty manifest should fail")
	}
	if err := (Manifest{Command: "vibe-acp", Cwd: "relative"}).Validate(); err == nil {
		t.Fatal("relative cwd should fail")
	}
	if err := (Manifest{Command: "vibe-acp", Env: []string{"FOO"}}).Validate(); err == nil {
		t.Fatal("env entry without = should fail")
	}
	if err := (Manifest{Command: "vibe-acp", Env: []string{"FOO=bar"}}).Validate(); err != nil {
		t.Fatalf("valid env entry rejected: %v", err)
	}
	if err := (Manifest{Command: "vibe-acp"}).Validate(); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

func TestRunnerRun(t *testing.T) {
	agent := &fakeAgent{reply: "PONG", systemPrompt: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.SystemPrompt = "be brief"
	var model string
	var started bool
	ctx := jobs.WithSubAgentModelPtr(context.Background(), &model)
	ctx = jobs.WithSubAgentStartNotifier(ctx, func(string) { started = true })
	var transcript []types.Message
	ctx = jobs.WithSubAgentTranscript(ctx, &transcript)
	var stop jobs.SubAgentStop
	ctx = jobs.WithSubAgentStop(ctx, &stop)
	var usage types.Usage
	ctx = jobs.WithSubAgentUsage(ctx, &usage)
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}

	got, err := r.Run(ctx, "hi", jobs.SubAgentParams{Role: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "PONG" {
		t.Fatalf("text = %q, want PONG", got)
	}
	if !started || model != "fake" {
		t.Fatalf("start notifier/model: started=%v model=%q", started, model)
	}
	if len(transcript) != 2 || transcript[0].Content != "hi" || transcript[1].Content != "PONG" {
		t.Fatalf("transcript = %+v", transcript)
	}
	if stop.FinishReason != "end_turn" {
		t.Fatalf("stop = %+v", stop)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", usage)
	}
	if agent.seenSystem == nil {
		t.Fatal("agent did not receive systemPrompt")
	}
}

func TestRunnerSystemPromptFallback(t *testing.T) {
	agent := &fakeAgent{reply: "ok"}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.SystemPrompt = "PREFIX"
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	if _, err := r.Run(context.Background(), "body", jobs.SubAgentParams{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if agent.seenSystem != nil {
		t.Fatal("agent should not receive systemPrompt when not advertised")
	}
	if !strings.HasPrefix(agent.seenPrompt, "PREFIX\n\nbody") {
		t.Fatalf("prompt = %q", agent.seenPrompt)
	}
}

func TestRunnerPermissionAutoAnswer(t *testing.T) {
	agent := &fakeAgent{reply: "allowed", askPermission: true, alwaysFirst: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	got, err := r.Run(context.Background(), "go", jobs.SubAgentParams{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "allowed" {
		t.Fatalf("text = %q", got)
	}
	if !agent.permissionGranted {
		t.Fatal("permission request was not auto-approved")
	}
}

func TestRunnerAllowAlwaysRequiresOptIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		permit bool
		want   string
	}{
		{"default", false, "allow"},
		{"opted_in", true, "always"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{reply: "ok", askPermission: true, alwaysFirst: true}
			r := New(Manifest{Name: "fake", Command: "fake"})
			r.PermitAllowAlways = tc.permit
			r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
				return wire(t, agent, c), nil
			}
			if _, err := r.Run(context.Background(), "go", jobs.SubAgentParams{}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if agent.permissionSelected != tc.want {
				t.Fatalf("selected %q, want %q", agent.permissionSelected, tc.want)
			}
		})
	}
}

func TestRunnerPermissionDenyPolicy(t *testing.T) {
	agent := &fakeAgent{reply: "denied", askPermission: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.AutoApprove = false
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	var denials []jobs.SubAgentDenial
	ctx := jobs.WithSubAgentDenials(context.Background(), &denials)
	got, err := r.Run(ctx, "go", jobs.SubAgentParams{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "denied" {
		t.Fatalf("text = %q", got)
	}
	if agent.permissionSelected != "reject" {
		t.Fatalf("selected = %q, want reject", agent.permissionSelected)
	}
	if len(denials) != 1 || !strings.Contains(denials[0].Reason, "deny") {
		t.Fatalf("denials = %+v", denials)
	}
}

func TestRunnerRefusalStopReason(t *testing.T) {
	agent := &fakeAgent{reply: "", stopReason: acp.StopReason("refusal")}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	var stop jobs.SubAgentStop
	ctx := jobs.WithSubAgentStop(context.Background(), &stop)
	if _, err := r.Run(ctx, "go", jobs.SubAgentParams{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stop.FinishReason != "refusal" {
		t.Fatalf("finish reason = %q", stop.FinishReason)
	}
}

func TestRunnerUsageMetaAliases(t *testing.T) {
	agent := &fakeAgent{reply: "ok", cachedPrompt: 7, totalTokens: 99}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	var usage types.Usage
	ctx := jobs.WithSubAgentUsage(context.Background(), &usage)
	if _, err := r.Run(ctx, "go", jobs.SubAgentParams{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if usage.CachedPromptTokens != 7 || usage.TotalTokens != 99 {
		t.Fatalf("usage = %+v, want cached 7 / total 99", usage)
	}
}

func TestRunnerEmitsReadyAndHeartbeats(t *testing.T) {
	agent := &fakeAgent{block: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.heartbeatInterval = 5 * time.Millisecond
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	hb := make(chan struct{}, 1)
	ready := false
	base := jobs.WithSubAgentHeartbeat(context.Background(), hb)
	base = jobs.WithSubAgentReadyNotifier(base, func() { ready = true })
	ctx, cancel := context.WithTimeout(base, 100*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, "sleep", jobs.SubAgentParams{}); err == nil {
		t.Fatal("expected deadline error")
	}
	if !ready {
		t.Fatal("ready notifier was not called")
	}
	select {
	case <-hb:
	default:
		t.Fatal("no heartbeat emitted during the turn")
	}
}

func TestRunnerMissingBinaryFailsAtStart(t *testing.T) {
	r := New(Manifest{Name: "missing", Command: "/definitely/not/a/real/acp-agent"})
	if _, err := r.Run(context.Background(), "hi", jobs.SubAgentParams{}); err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("expected a start error, got %v", err)
	}
}

func TestRunnerDeadlineIsNotASupervisorHalt(t *testing.T) {
	agent := &fakeAgent{block: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	var stop jobs.SubAgentStop
	ctx := jobs.WithSubAgentStop(context.Background(), &stop)
	deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := r.Run(deadline, "sleep", jobs.SubAgentParams{}); err == nil {
		t.Fatal("expected deadline error")
	}
	if stop.HaltedBySupervisor || stop.FinishReason != "timeout" {
		t.Fatalf("stop = %+v, want timeout without supervisor halt", stop)
	}
}

func TestRunnerOutputLimitKeepsUTF8Intact(t *testing.T) {
	agent := &fakeAgent{reply: strings.Repeat("é", 20)}
	r := New(Manifest{Name: "fake", Command: "fake"})
	r.connect = func(_ context.Context, c acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	got, err := r.Run(context.Background(), "go", jobs.SubAgentParams{OutputLimit: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated report is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "éé\n...[truncated by acpclient]...") {
		t.Fatalf("report = %q", got)
	}
}

func TestRunnerCancel(t *testing.T) {
	agent := &fakeAgent{block: true}
	r := New(Manifest{Name: "fake", Command: "fake"})
	c := &collector{}
	r.connect = func(_ context.Context, _ acp.Client, _ string) (*connection, error) {
		return wire(t, agent, c), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, "sleep", jobs.SubAgentParams{}); err == nil {
		t.Fatal("expected cancellation error")
	}
}
