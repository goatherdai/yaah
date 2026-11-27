package acpclient

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/goatherdai/yaah/internal/jobs"
	"github.com/goatherdai/yaah/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const maxStderrBytes = 64 << 10

// limitedBuffer keeps the agent's stderr bounded for error messages.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxStderrBytes - b.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, _ := b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// connection is a live client connection plus its teardown and stderr tail.
type connection struct {
	conn   *acp.ClientSideConnection
	close  func()
	stderr func() string
}

func stageErr(stage string, err error, stderr func() string) error {
	if err == nil {
		return nil
	}
	tail := stderr()
	if tail == "" {
		return fmt.Errorf("acpclient: %s: %w", stage, err)
	}
	return fmt.Errorf("acpclient: %s: %w (stderr: %s)", stage, err, tail)
}

// Runner adapts one external ACP agent to the jobs.TaskRunner contract.
//
// Session policy: one Run == one child process == one ACP connection == one
// session. Nothing is reused between calls, so a dispatch cannot inherit
// another dispatch's conversation or tools. The close function always closes
// stdin, kills the process, and waits — on success, error, cancellation, and
// timeout alike — so a child is never left behind.
type Runner struct {
	Manifest Manifest

	// SystemPrompt, when set, is delivered through ACP's systemPrompt field
	// when the agent advertises that capability, and otherwise prepended to
	// the prompt text.
	SystemPrompt string

	// AutoApprove selects the first allow option for permission requests. It
	// defaults to true; yaah sub-agents have no approval bridge.
	AutoApprove bool

	// connect is an unexported seam for deterministic in-process tests.
	connect func(context.Context, acp.Client, string) (*connection, error)
}

// New returns a Runner with the sub-agent-safe defaults.
func New(m Manifest) *Runner { return &Runner{Manifest: m, AutoApprove: true} }

// TaskRunner returns the runner as the contract consumed by delegate and
// workflow dispatch.
func (r *Runner) TaskRunner() jobs.TaskRunner { return r.Run }

func (r *Runner) cwd(params jobs.SubAgentParams) (string, error) {
	cwd := params.Workdir
	if cwd == "" {
		cwd = r.Manifest.Cwd
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("acpclient: resolve cwd: %w", err)
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("acpclient: absolute cwd: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("acpclient: cwd %q is not a directory", abs)
	}
	return abs, nil
}

func (r *Runner) connectChild(ctx context.Context, c acp.Client, cwd string) (*connection, error) {
	cmd := exec.CommandContext(ctx, r.Manifest.Command, r.Manifest.Args...)
	cmd.Dir = cwd
	if len(r.Manifest.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Manifest.Env...)
	}
	stderr := &limitedBuffer{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acpclient: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acpclient: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acpclient: start %q: %w", r.Manifest.Command, err)
	}
	conn := acp.NewClientSideConnection(c, stdin, stdout)
	closeFn := func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	return &connection{conn: conn, close: closeFn, stderr: stderr.String}, nil
}

// Run drives one prompt turn in the external agent and returns its final text.
func (r *Runner) Run(ctx context.Context, prompt string, params jobs.SubAgentParams) (string, error) {
	if err := r.Manifest.Validate(); err != nil {
		return "", err
	}
	cwd, err := r.cwd(params)
	if err != nil {
		return "", err
	}
	if params.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	collector := &collector{autoApprove: r.AutoApprove}
	var conn *connection
	if r.connect != nil {
		conn, err = r.connect(ctx, collector, cwd)
	} else {
		conn, err = r.connectChild(ctx, collector, cwd)
	}
	if err != nil {
		return "", err
	}
	defer conn.close()

	init, err := conn.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "yaah-acpclient", Version: "0"},
	})
	if err != nil {
		return "", stageErr("initialize", err, conn.stderr)
	}

	turnPrompt := prompt
	req := acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}}
	if r.SystemPrompt != "" {
		if init.AgentCapabilities.SessionCapabilities.SystemPrompt != nil {
			req.SystemPrompt = &acp.SystemPrompt{
				Mode:    acp.SystemPromptModeAppend,
				Content: []acp.ContentBlock{acp.TextBlock(r.SystemPrompt)},
			}
		} else {
			turnPrompt = r.SystemPrompt + "\n\n" + prompt
		}
	}

	session, err := conn.conn.NewSession(ctx, req)
	if err != nil {
		return "", stageErr("session/new", err, conn.stderr)
	}

	model := params.Model
	if model == "" {
		model = r.Manifest.Name
	}
	if model == "" {
		model = r.Manifest.Command
	}
	jobs.WriteSubAgentModel(ctx, model)
	jobs.NotifySubAgentStart(ctx, model)

	resp, err := conn.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(turnPrompt)},
	})
	if err != nil {
		if ctx.Err() != nil {
			jobs.CaptureSubAgentStop(ctx, jobs.SubAgentStop{HaltedBySupervisor: true, FinishReason: "cancelled"})
		}
		return "", stageErr("session/prompt", err, conn.stderr)
	}

	text, chunks, calls, permissions, updates, usage, denials := collector.result()
	jobs.AddSubAgentUsage(ctx, tokensFromUsage(usage))
	jobs.CaptureSubAgentDenials(ctx, denials)
	jobs.CaptureSubAgentTranscript(ctx, []types.Message{
		{Role: "user", Content: turnPrompt},
		{Role: "assistant", Content: text},
	})
	stop := jobs.SubAgentStop{FinishReason: string(resp.StopReason)}
	switch string(resp.StopReason) {
	case "cancelled":
		stop.HaltedBySupervisor = true
	case "max_turn_requests":
		stop.TurnBudgetExhausted = true
	}
	jobs.CaptureSubAgentStop(ctx, stop)

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		attrs := []attribute.KeyValue{
			attribute.String("acp.agent", r.Manifest.Name),
			attribute.String("acp.session_id", string(session.SessionId)),
			attribute.String("acp.stop_reason", string(resp.StopReason)),
			attribute.Int("acp.updates", updates),
			attribute.Int("acp.message_chunks", chunks),
			attribute.Int("acp.tool_calls", calls),
			attribute.Int("acp.permission_requests", permissions),
			attribute.Int("acp.denials", len(denials)),
		}
		if cost, ok := usage.Meta["cost"]; ok {
			attrs = append(attrs, attribute.String("acp.cost", string(cost)))
		}
		span.SetAttributes(attrs...)
	}

	if params.OutputLimit > 0 && len(text) > params.OutputLimit {
		text = text[:params.OutputLimit] + "\n...[truncated by acpclient]..."
	}
	return text, nil
}

// tokensFromUsage maps ACP usage_update metadata to yaah's usage shape. ACP
// reports cumulative counts, so the caller should add this once per turn.
func tokensFromUsage(u acp.SessionUsageUpdate) types.Usage {
	var out types.Usage
	var prompt, completion, cached int64
	if err := u.Meta.Get("promptTokens", &prompt); err == nil {
		out.PromptTokens = int(prompt)
	}
	if err := u.Meta.Get("completionTokens", &completion); err == nil {
		out.CompletionTokens = int(completion)
	}
	if err := u.Meta.Get("cachedTokens", &cached); err == nil {
		out.CachedPromptTokens = int(cached)
	}
	out.TotalTokens = out.PromptTokens + out.CompletionTokens
	return out
}
