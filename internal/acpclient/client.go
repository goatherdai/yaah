package acpclient

import (
	"context"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/goatherdai/yaah/internal/jobs"
)

// collector is the client side of an ACP connection. It turns the agent's
// streamed updates into one final text result and auto-answers permission
// requests, matching the allow-mode posture of yaah's in-process sub-agents.
type collector struct {
	mu            sync.Mutex
	text          strings.Builder
	updates       int
	messageChunks int
	toolCalls     int
	permissions   int
	lastUsage     acp.SessionUsageUpdate
	autoApprove   bool
	denials       []jobs.SubAgentDenial
}

var _ acp.Client = (*collector)(nil)

func (c *collector) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates++
	u := p.Update
	switch {
	case u.AgentMessageChunk != nil:
		c.messageChunks++
		if block := u.AgentMessageChunk.Content.Text; block != nil {
			c.text.WriteString(block.Text)
		}
	case u.ToolCall != nil:
		c.toolCalls++
	case u.UsageUpdate != nil:
		c.lastUsage = *u.UsageUpdate
	}
	return nil
}

func (c *collector) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	tool, _ := p.ToolCall.Name.Value()
	if tool == "" {
		tool = "acp-tool"
	}
	auto := c.autoApprove
	c.mu.Lock()
	c.permissions++
	c.mu.Unlock()

	// Select an option by kind, never by label: the agent chooses both the
	// id and the polarity, so a label cannot be trusted to mean "allow".
	selectByKind := func(kinds ...acp.PermissionOptionKind) (acp.PermissionOptionId, bool) {
		for _, option := range p.Options {
			for _, kind := range kinds {
				if option.Kind == kind {
					return option.OptionId, true
				}
			}
		}
		return "", false
	}

	allowKinds := []acp.PermissionOptionKind{acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways}
	rejectKinds := []acp.PermissionOptionKind{acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways}

	if auto {
		if id, ok := selectByKind(allowKinds...); ok {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(id)}, nil
		}
		if id, ok := selectByKind(rejectKinds...); ok {
			c.recordDenial(tool, "permission request offered no allow option")
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(id)}, nil
		}
	} else if id, ok := selectByKind(rejectKinds...); ok {
		c.recordDenial(tool, "acpclient permission policy is deny")
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(id)}, nil
	}
	return acp.RequestPermissionResponse{}, acp.NewInvalidParams(map[string]any{
		"error": "acpclient: permission request offered no selectable option",
	})
}

func (c *collector) recordDenial(tool, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denials = append(c.denials, jobs.SubAgentDenial{Tool: tool, Reason: reason})
}

// result returns a snapshot of everything collected during the turn.
func (c *collector) result() (text string, chunks, calls, permissions, updates int, usage acp.SessionUsageUpdate, denials []jobs.SubAgentDenial) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String(), c.messageChunks, c.toolCalls, c.permissions, c.updates, c.lastUsage, append([]jobs.SubAgentDenial(nil), c.denials...)
}

func notFound(method string) *acp.RequestError { return acp.NewMethodNotFound(method) }

// The remaining Client methods are capabilities yaah does not offer to an
// external agent in this phase. Returning method-not-found keeps them out of
// ClientCapabilities, so a conformant agent never calls them.
func (c *collector) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return notFound("elicitation/complete")
}
func (c *collector) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, notFound("elicitation/create")
}
func (c *collector) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, notFound("fs/read_text_file")
}
func (c *collector) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, notFound("fs/write_text_file")
}
func (c *collector) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, notFound("terminal/create")
}
func (c *collector) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, notFound("terminal/kill")
}
func (c *collector) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, notFound("terminal/output")
}
func (c *collector) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, notFound("terminal/release")
}
func (c *collector) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, notFound("terminal/wait_for_exit")
}
