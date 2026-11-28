package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goatherdai/yaah/internal/jsonrpc"
	"github.com/goatherdai/yaah/internal/permissions"
)

// clientResponse is the routed outcome of one server-initiated request.
type clientResponse struct {
	err    error // non-nil when the client answered with an error object
	result json.RawMessage
}

// clientRequestError preserves the JSON-RPC code so callers can distinguish a
// semantic refusal (for example -32603 from a client that will not write a
// path) from transport failure and render an actionable message.
type clientRequestError struct {
	Code    int
	Message string
}

func (e *clientRequestError) Error() string {
	return fmt.Sprintf("client error %d: %s", e.Code, e.Message)
}

// permissionID canonicalizes a numeric request id for the routing map.
func permissionID(n int) string { return fmt.Sprintf("%d", n) }

// resolvePending routes a client response to the waiting request by
// id, if any is pending. Responses for unknown ids are dropped.
func (s *Server) resolvePending(msg jsonrpc.Message) {
	key := s.canonicalID(msg.ID)
	s.mu.Lock()
	ch, ok := s.pendingReq[key]
	delete(s.pendingReq, key)
	s.mu.Unlock()
	if !ok {
		s.logf("response for unknown request id %s", key)
		return
	}
	out := clientResponse{}
	if msg.Error != nil {
		out.err = &clientRequestError{Code: msg.Error.Code, Message: msg.Error.Message}
	} else {
		out.result = msg.Result
	}
	select {
	case ch <- out:
	default:
	}
}

// toolKind maps a yaah tool name onto an ACP tool kind for the client's
// icon and presentation treatment. Unlisted tools are "other".
func toolKind(name string) string {
	switch name {
	case "read", "grep", "glob", "ls", "memory", "prompt":
		return KindRead
	case "write", "edit", "patch", "replace", "go_refactor", "apply_out":
		return KindEdit
	case "delete":
		return KindDelete
	case "bash", "powershell", "git", "process", "background", "go_mod", "go_test", "staticcheck", "bisect":
		return KindExecute
	case "webfetch", "http", "web_search", "fetch":
		return KindFetch
	case "question", "think":
		return KindThink
	case "todo", "todowrite", "plan":
		return KindEdit
	default:
		return KindOther
	}
}

// requestPermission implements the agent loop's ApproveFn for ACP: it
// reports the pending tool call, sends session/request_permission to
// the client, and blocks (bounded) for the decision.
//
// The approval middleware runs BEFORE the tool's own ToolStartEvent,
// so the pending call is carried entirely by the permission request;
// a denial is rendered as a failed tool_call_update so the user sees
// the outcome, and an approval is followed by the real tool_call from
// the dispatch pipeline.
func (s *Server) requestPermission(
	sessionID, callID, name, args string,
	runCtx context.Context,
	c *connection,
) bool {
	// A previous allow_always/reject_always decision short-circuits the
	// client round-trip: an always-allowed tool proceeds, an
	// always-rejected one is denied, both without re-prompting for the
	// rest of the session. No client row is created in either case —
	// an approval is followed by the pipeline's real tool_call, and a
	// denial surfaces through the agent's own failed tool result.
	s.mu.Lock()
	st := s.sessions[sessionID]
	var remembered string
	if st != nil {
		// Session memory is keyed by tool name only, so an "always" granted
		// for `edit src/main.go` would otherwise lift a later `edit .env`
		// without ever reaching the ask. The carve-out holds here too
		// (yaah-x6vi.7): a credential-shaped target is never lifted from
		// the memory.
		if st.permAllow[name] && !permissions.SensitiveCall(name, args) {
			remembered = PermAllowAlways
		} else if st.permDeny[name] {
			remembered = PermRejectAlways
		}
	}
	s.mu.Unlock()
	switch remembered {
	case PermAllowAlways:
		s.logf("permission auto-allowed for session %s: %s (always)", sessionID, name)
		return true
	case PermRejectAlways:
		s.logf("permission auto-denied for session %s: %s (always)", sessionID, name)
		return false
	}

	// c == nil means Run has not started (or already ended): deny,
	// never block. requestClient owns id minting, answer registration,
	// the send and the bounded wait.
	if c == nil {
		return false
	}

	toolCallID := callID
	announced := callID != ""
	title := permissionTitle(name, args)
	if title == "" {
		title = name
	}
	timeout := s.answerTimeout()

	params := &RequestPermissionParams{
		SessionID: sessionID,
		ToolCall: ToolCallUpdate{
			SessionUpdate: UpdateToolCallUpdate,
			Title:         title,
			Name:          name,
			Kind:          toolKind(name),
			Status:        StatusPending,
		},
		Options: permissionOptions(name),
	}

	// Announce the row after the answer channel is registered but before
	// the request goes out, so a fast client cannot answer a row it has
	// not seen. The minted id also supplies the synthetic fallback for a
	// model that gave no call id.
	beforeSend := func(id int) {
		if toolCallID == "" {
			toolCallID = fmt.Sprintf("perm_%d", id)
		}
		params.ToolCall.ToolCallID = toolCallID
		if !announced {
			return
		}
		s.mu.Lock()
		st := s.sessions[sessionID]
		if st != nil && st.view != nil {
			st.view.Announce(toolCallID)
		}
		s.mu.Unlock()
		c.sendUpdate(sessionID, ToolCallUpdate{
			SessionUpdate: UpdateToolCall,
			ToolCallID:    toolCallID,
			Title:         title,
			Name:          name,
			Kind:          toolKind(name),
			Status:        StatusPending,
		})
	}

	deny := func(reason string) {
		s.logf("permission %s denied: %s", toolCallID, reason)
		// Render the denial so a row the client created from the
		// permission request does not hang in a pending state. The
		// id-less fallback (no announced row) renders nothing: a failed
		// update for a synthetic perm_N id the client never saw would
		// create an orphan row instead.
		if !announced {
			return
		}
		c.sendUpdate(sessionID, ToolCallUpdate{
			SessionUpdate: UpdateToolCallUpdate,
			ToolCallID:    toolCallID,
			Title:         title,
			Name:          name,
			Kind:          toolKind(name),
			Status:        StatusFailed,
			Content: []UpdateContent{{
				Type:    "content",
				Content: &ContentBlock{Type: "text", Text: "Denied: " + reason},
			}},
		})
	}

	resp, outcome, err := s.requestClient(runCtx, c, "session/request_permission", params,
		clientRequestOptions{Timeout: timeout, BeforeSend: beforeSend})
	if err != nil {
		return false
	}
	switch outcome {
	case clientTimedOut:
		deny("no answer from client within " + timeout.String())
		return false
	case clientTurnEnded:
		// The turn is over (cancel or shutdown); the spec requires a
		// cancelled outcome from the client in this state, but a local
		// deny is the safe interpretation either way. No failed-row
		// render here: the client is unwinding the same turn.
		s.logf("permission request %s abandoned: turn ended", toolCallID)
		return false
	}

	if resp.err != nil {
		deny(resp.err.Error())
		return false
	}
	if resp.result == nil {
		deny("empty response from client")
		return false
	}
	var out RequestPermissionResult
	if err := json.Unmarshal(resp.result, &out); err != nil {
		deny("unparseable response: " + err.Error())
		return false
	}
	if out.Outcome.Outcome != "selected" {
		deny("not selected")
		return false
	}
	switch out.Outcome.OptionID {
	case "allow-once":
		return true
	case "allow-always":
		// A credential-shaped target downgrades to allow-once
		// (yaah-x6vi.7, the kilocode protected-path downgrade): neither
		// the session's tool-keyed memory nor the project store records
		// it, so the next sensitive call re-asks. The test is on the call
		// rather than on ResourceFor's one derived string, so a tool that
		// hides its target in another argument (replace/bulk_files select
		// files with `pattern`; patch may name only a diff body) is
		// covered too. Say so out loud — the user clicked "always", and
		// the next call asking again with no explanation reads as a bug.
		if permissions.SensitiveCall(name, args) {
			s.logf("permission: %s targets a credential-shaped file; not remembered (allow-once only)", name)
			return true
		}
		s.rememberPermission(sessionID, name, PermAllowAlways)
		// Persist the decision project-scoped so a later session in the
		// same workspace does not re-ask (yaah-x6vi.6). The resource is the
		// ask's own derivation — the same string the consult matches — and
		// id-less asks (out-of-workspace path decisions) are never savable:
		// a prompt that did not carry a tool call carries no resource to
		// save. A failure logs and stays session-scoped; the ask is not
		// re-fired either way.
		if callID != "" && s.SaveApproval != nil {
			if err := s.SaveApproval(st.cwd, name, permissions.ResourceFor(name, args)); err != nil {
				s.logf("save approval %s/%s: %v", st.cwd, name, err)
			}
		}
		return true
	case "reject-once":
		deny("rejected by user")
		return false
	case "reject-always":
		s.rememberPermission(sessionID, name, PermRejectAlways)
		deny("always rejected for this session")
		return false
	default:
		deny("unknown option " + out.Outcome.OptionID)
		return false
	}
}

// permissionOptions is the full decision menu Zed renders as "Allow
// once / Always for <tool> / Reject / Always for <pattern>"-style
// buttons; the always kinds only enable those buttons when present.
func permissionOptions(name string) []PermissionOption {
	return []PermissionOption{
		{OptionID: "allow-once", Name: "Allow once", Kind: PermAllowOnce},
		{OptionID: "allow-always", Name: "Always allow " + name, Kind: PermAllowAlways},
		{OptionID: "reject-once", Name: "Reject", Kind: PermRejectOnce},
		{OptionID: "reject-always", Name: "Always reject " + name, Kind: PermRejectAlways},
	}
}

// rememberPermission records an always-kind decision for the rest of
// the session; the entry dies with the session (retireSession), never
// in the store.
func (s *Server) rememberPermission(sessionID, name, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	switch kind {
	case PermAllowAlways:
		if st.permAllow == nil {
			st.permAllow = make(map[string]bool)
		}
		st.permAllow[name] = true
	case PermRejectAlways:
		if st.permDeny == nil {
			st.permDeny = make(map[string]bool)
		}
		st.permDeny[name] = true
	}
}

// permissionTitle renders a short human-readable title for a pending
// call: the tool name plus a compacted argument hint.
func permissionTitle(name, args string) string {
	hint := strings.Join(strings.Fields(args), " ")
	if hint == "" {
		return name
	}
	return fmt.Sprintf("%s: %s", name, truncate(hint, 60))
}
