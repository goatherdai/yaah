// Package jsonrpc carries the JSON-RPC 2.0 wire layer shared by yaah's two
// RPC surfaces: the MCP client and server (internal/mcp) and the ACP server
// (internal/acp). It is a leaf package — stdlib imports only — owning the
// wire types, the reserved error codes, and the stdio framings
// (Content-Length and newline-delimited, plus the reader that auto-detects
// between them). Protocol semantics stay in their packages: MCP's
// initialize handshake and tools/* methods in internal/mcp, ACP's session
// lifecycle and method dispatch in internal/acp.
package jsonrpc

import "encoding/json"

// Version is the protocol version string every message carries.
const Version = "2.0"

// Message is a JSON-RPC 2.0 request, response, or notification. ID is kept
// raw so responses echo the peer's id exactly — number or string, whatever
// spelling the peer used. Callers that only mint numeric ids (the MCP
// client's pending-map correlation) use NewRequest and IDInt instead of
// touching the raw field.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// The reserved JSON-RPC 2.0 error codes. Server-defined codes (ACP's -32002
// busy, MCP's -32000 connection-lost) stay with the protocol that defines
// them.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// NewRequest builds a request carrying a numeric id, the form every
// jsonrpc-speaker in this repo mints when it initiates a call.
func NewRequest(id int64, method string, params json.RawMessage) Message {
	raw, err := json.Marshal(id)
	if err != nil {
		// int64 marshaling cannot fail; keep the API clean rather than
		// surfacing an impossible error.
		return Message{JSONRPC: Version, Method: method, Params: params}
	}
	return Message{JSONRPC: Version, ID: raw, Method: method, Params: params}
}

// NewNotification builds a notification: no id, so no response will ever
// arrive for it.
func NewNotification(method string, params json.RawMessage) Message {
	return Message{JSONRPC: Version, Method: method, Params: params}
}

// IsNotification reports whether the message carries no id. JSON-RPC 2.0
// notifications are exactly the id-less messages, and they never get a
// response.
func (m Message) IsNotification() bool { return len(m.ID) == 0 }

// IDInt decodes a numeric id. The second return is false for absent ids,
// for the raw `null` id (spec parse-error responses carry it), and for
// string ids — a caller correlating responses by number (the MCP client's
// pending map) treats those as unroutable rather than as id 0. The null
// guard is load-bearing: unmarshaling `null` into an int is a documented
// no-op, so without it a parse-error response would masquerade as a
// correlated response for id 0.
func (m Message) IDInt() (int64, bool) {
	if len(m.ID) == 0 || string(m.ID) == "null" {
		return 0, false
	}
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return 0, false
	}
	return id, true
}
