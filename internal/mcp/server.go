package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/goatherdai/yaah/internal/jsonrpc"
	"io"
)

// ToolHandler processes a tool call and returns the result content.
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// ServerToolDef defines a tool exposed by the server.
type ServerToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for the tool's parameters
	Handler     ToolHandler
}

// Server is an MCP tool server that communicates over stdio using
// JSON-RPC 2.0. It auto-detects Content-Length or newline-delimited
// framing from the first inbound message and mirrors it on output.
type Server struct {
	name    string
	version string
	tools   []ServerToolDef
	reader  messageReader
	writer  messageWriter
}

// NewServer creates a new MCP server with the given name and version.
func NewServer(name, version string) *Server {
	return &Server{
		name:    name,
		version: version,
	}
}

// AddTool registers a tool definition with the server.
func (s *Server) AddTool(def ServerToolDef) {
	s.tools = append(s.tools, def)
}

// Serve reads JSON-RPC messages from in, handles them, and writes responses
// to out. It blocks until the input stream is closed (EOF) or the context is
// cancelled. Framing is auto-detected from the first inbound message:
// Content-Length (MCP spec default) or newline-delimited JSON.
//
// Reading happens in a dedicated goroutine so context cancellation is honored
// promptly even while blocked waiting for the next stdin line. On cancellation
// the reader goroutine is left blocked on the (now-abandoned) input; the
// process is expected to exit shortly after Serve returns.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	detector := jsonrpc.NewAutoDetectReader(in)
	s.reader = detector

	type readResult struct {
		msg jsonrpc.Message
		err error
	}
	msgCh := make(chan readResult)

	go func() {
		for {
			msg, err := detector.ReadMessage()
			msgCh <- readResult{msg, err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-msgCh:
			if r.err != nil {
				if r.err == io.EOF {
					return nil
				}
				return fmt.Errorf("read message: %w", r.err)
			}

			// Lazily create the writer once framing is known. The
			// detector goroutine hands this goroutine the message over
			// msgCh, which is the synchronization edge that makes its
			// Framing() report safe to read here.
			if s.writer == nil {
				if detector.Framing() == jsonrpc.FramingContentLength {
					s.writer = jsonrpc.NewFramedWriter(out)
				} else {
					s.writer = jsonrpc.NewNewlineWriter(out)
				}
			}

			msg := r.msg
			// Notifications carry no id (empty raw id after unmarshal).
			isNotification := msg.IsNotification()

			result, rpcErr := s.dispatch(ctx, msg)

			// Don't send responses for notifications
			if isNotification {
				continue
			}

			resp := jsonrpc.Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
			}
			if rpcErr != nil {
				resp.Error = rpcErr
			} else {
				resp.Result = result
			}

			if err := s.writer.WriteMessage(resp); err != nil {
				return fmt.Errorf("write response: %w", err)
			}
		}
	}
}

// dispatch routes a JSON-RPC message to the appropriate handler based on
// the method field. It returns either a result (as raw JSON) or a
// JSON-RPC error.
func (s *Server) dispatch(ctx context.Context, msg jsonrpc.Message) (json.RawMessage, *jsonrpc.Error) {
	switch msg.Method {
	case "initialize":
		return s.handleInitialize(msg.Params)
	case "notifications/initialized":
		return nil, nil // notification — no response needed
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, msg.Params)
	case "ping":
		return json.RawMessage(`{}`), nil
	default:
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: fmt.Sprintf("Method not found: %s", msg.Method),
		}
	}
}

// handleInitialize returns the server capabilities and identity. The
// protocol version is negotiated by echoing back the version the
// client requested, falling back to yaah's default (2024-11-05) if
// the client omits one. This matches the behavior of
// @modelcontextprotocol/sdk servers and keeps the wire compatible
// with clients that request newer protocol revisions.
func (s *Server) handleInitialize(params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	const defaultVersion = "2024-11-05"
	version := defaultVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]bool{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]string{
			"name":    s.name,
			"version": s.version,
		},
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: "internal error"}
	}
	return data, nil
}

// toolEntry mirrors the JSON structure of a tool in the tools/list response.
type toolEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// handleToolsList returns the list of registered tool definitions.
func (s *Server) handleToolsList() (json.RawMessage, *jsonrpc.Error) {
	tools := make([]toolEntry, len(s.tools))
	for i, t := range s.tools {
		schema := t.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools[i] = toolEntry{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		}
	}
	result := struct {
		Tools []toolEntry `json:"tools"`
	}{Tools: tools}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: "internal error"}
	}
	return data, nil
}

// toolsCallParams is the JSON structure of a tools/call request params.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolsCall finds the named tool and invokes its handler, returning
// a content result. If the handler returns an error, it is reported in the
// content with isError: true rather than as a JSON-RPC error.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	if params == nil {
		return s.makeToolError("missing params"), nil
	}

	var p toolsCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}

	// Find the tool by name
	var tool *ServerToolDef
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			tool = &s.tools[i]
			break
		}
	}

	if tool == nil {
		return s.makeToolError("unknown tool: " + p.Name), nil
	}

	text, err := tool.Handler(ctx, p.Arguments)
	if err != nil {
		return s.makeToolError(err.Error()), nil
	}

	result, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	return result, nil
}

// makeToolError formats a tool execution error as an MCP content result
// with isError: true.
func (s *Server) makeToolError(msg string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "error: " + msg},
		},
		"isError": true,
	})
	return data
}
