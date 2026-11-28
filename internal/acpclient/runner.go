package acpclient

import (
	"bytes"
	"context"
	"errors"
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
	consumed := len(p)
	remaining := maxStderrBytes - b.buf.Len()
	if remaining <= 0 {
		return consumed, nil
	}
	kept := p
	if len(kept) > remaining {
		kept = kept[:remaining]
	}
	_, _ = b.buf.Write(kept)
	// Always report the full input: io.Copy treats a short write with no
	// error as ErrShortWrite and would stop draining the child's stderr.
	return consumed, nil
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
// stdin, kills the direct child, and waits on success, error, cancellation,
// and timeout alike. Descendants the agent spawns itself are its own
// responsibility: yaah does not manage another process group on Windows.
type Runner struct {
	Manifest Manifest

	// SystemPrompt, when set, is delivered through ACP's systemPrompt field
	// when the agent advertises that capability, and otherwise prepended to
	// the prompt text.
	SystemPrompt string

	// AutoApprove selects an allow option for permission requests. It
	// defaults to true; yaah sub-agents have no approval bridge. Only
	// allow_once is selected unless PermitAllowAlways is set.
	AutoApprove bool

	// PermitAllowAlways opts into persistent (allow_always) grants. Off by
	// default because an external agent's tool calls bypass yaah's approval
	// middleware.
	PermitAllowAlways bool

	// connect is an unexported seam for deterministic in-process tests.
	connect func(context.Context, acp.Client, string) (*connection, error)

	// heartbeatInterval is unexported so tests can shrink the 5s watchdog beat.
	heartbeatInterval time.Duration
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

// classifyCtxStop records the run's stop classification when an error
// ends the turn, whatever stage it failed at: a deadline is the run
// budget's timeout — retainedStopReason checks HaltedBySupervisor first,
// so leave that clear here — and any other cancellation is a supervisor
// halt. A stage failure with a live context records nothing: the
// envelope classifies how a started run ended, not why it never started.
func classifyCtxStop(ctx context.Context, err error) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		jobs.CaptureSubAgentStop(ctx, jobs.SubAgentStop{FinishReason: "timeout"})
	case ctx.Err() != nil:
		jobs.CaptureSubAgentStop(ctx, jobs.SubAgentStop{HaltedBySupervisor: true, FinishReason: "cancelled"})
	}
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

	collector := &collector{autoApprove: r.AutoApprove, permitAllowAlways: r.PermitAllowAlways}
	var conn *connection
	if r.connect != nil {
		conn, err = r.connect(ctx, collector, cwd)
	} else {
		conn, err = r.connectChild(ctx, collector, cwd)
	}
	if err != nil {
		classifyCtxStop(ctx, err)
		return "", err
	}
	defer conn.close()

	init, err := conn.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "yaah-acpclient", Version: "0"},
	})
	if err != nil {
		classifyCtxStop(ctx, err)
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
		classifyCtxStop(ctx, err)
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

	// The stuck-child watchdog arms on "ready" and expects periodic beats.
	// The in-process loop beats once per iteration; a single long ACP turn
	// has no iterations, so beat on a timer until the turn ends.
	jobs.NotifySubAgentReady(ctx)
	jobs.SendHeartbeat(ctx)
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	interval := r.heartbeatInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				jobs.SendHeartbeat(ctx)
			}
		}
	}()

	resp, err := conn.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(turnPrompt)},
	})
	if err != nil {
		classifyCtxStop(ctx, err)
		return "", stageErr("session/prompt", err, conn.stderr)
	}

	text, chunks, calls, permissions, updates, usage, denials, permissionKinds := collector.result()
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
			attribute.StringSlice("acp.permission_kinds", permissionKinds),
		}
		if cost, ok := usage.Meta["cost"]; ok {
			attrs = append(attrs, attribute.String("acp.cost", string(cost)))
		}
		span.SetAttributes(attrs...)
	}

	if params.OutputLimit > 0 && len(text) > params.OutputLimit {
		text = truncateUTF8Bytes(text, params.OutputLimit) + "\n...[truncated by acpclient]..."
	}
	return text, nil
}

// truncateUTF8Bytes mirrors the in-process runner's safeTruncateBytes so a
// cap never splits a UTF-8 rune in the report handed back to the parent.
func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	i := maxBytes
	for i > 0 && i < len(s) {
		if s[i] < 0x80 || s[i]&0xC0 == 0xC0 {
			break
		}
		i--
	}
	return s[:i]
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
	// yaah's ACP server emits cachedPromptTokens; Vibe emits cachedTokens.
	// Accept both so driving either agent records the same usage shape.
	if err := u.Meta.Get("cachedPromptTokens", &cached); err != nil {
		_ = u.Meta.Get("cachedTokens", &cached)
	}
	out.CachedPromptTokens = int(cached)
	var total int64
	if err := u.Meta.Get("totalTokens", &total); err == nil && total > 0 {
		out.TotalTokens = int(total)
	} else {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}
