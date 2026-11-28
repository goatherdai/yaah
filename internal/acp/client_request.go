package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// clientRequestOptions controls one server-initiated JSON-RPC request.
type clientRequestOptions struct {
	// Timeout bounds the wait for the client's answer. Zero means the
	// context alone bounds it; callers wanting the shared control-plane
	// answer timeout pass answerTimeout() explicitly.
	Timeout time.Duration

	// BeforeSend runs after the answer channel is registered but before
	// the request goes out. It receives the minted id so the permission
	// bridge can announce a matching row without racing a fast client.
	BeforeSend func(id int)

	// Late, when non-nil, receives a successful answer that arrives after
	// ctx ended. The pending entry stays routed for a bounded window so a
	// resource-bearing request (terminal/create) can be released.
	Late func(json.RawMessage)
}

// clientRequestOutcome distinguishes an answer from the two ways a wait can
// end without one. Each caller renders its own surface-specific failure.
type clientRequestOutcome int

const (
	clientAnswered clientRequestOutcome = iota
	clientTimedOut
	clientTurnEnded
)

// lateAnswerWindow bounds how long a late answer can still be routed to a
// Late hook after the caller gave up.
const lateAnswerWindow = 30 * time.Second

// answerTimeout resolves the shared control-plane answer bound. approval,
// elicitation and the TUI/web control bridges all wait this long.
func (s *Server) answerTimeout() time.Duration {
	if s.PermissionTimeout > 0 {
		return s.PermissionTimeout
	}
	return defaultPermissionTimeout
}

// requestClient is the single outbound ACP request lifecycle: mint an id,
// register the answer channel BEFORE sending, send, then wait for the answer,
// the timeout or the caller's context. The pending entry is removed on every
// exit path, except while a Late hook is waiting for a post-deadline answer.
func (s *Server) requestClient(
	ctx context.Context,
	c *connection,
	method string,
	params any,
	opts clientRequestOptions,
) (clientResponse, clientRequestOutcome, error) {
	if c == nil {
		return clientResponse{}, clientAnswered, fmt.Errorf("%s: no client connection", method)
	}

	// One lock covers both id minting and registration: a client cannot
	// answer before resolvePending can find the channel.
	s.mu.Lock()
	s.nextReqID++
	id := s.nextReqID
	key := permissionID(id)
	answer := make(chan clientResponse, 1)
	s.pendingReq[key] = answer
	s.mu.Unlock()

	keepPending := false
	defer func() {
		if keepPending {
			return
		}
		s.mu.Lock()
		delete(s.pendingReq, key)
		s.mu.Unlock()
	}()

	if opts.BeforeSend != nil {
		opts.BeforeSend(id)
	}
	c.sendRequest(id, method, params)

	var timeoutC <-chan time.Time
	if opts.Timeout > 0 {
		timer := time.NewTimer(opts.Timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}
	var ctxC <-chan struct{}
	if ctx != nil {
		ctxC = ctx.Done()
	}

	select {
	case resp := <-answer:
		return resp, clientAnswered, nil
	case <-timeoutC:
		return clientResponse{}, clientTimedOut, nil
	case <-ctxC:
		if opts.Late != nil {
			keepPending = true
			go s.awaitLate(answer, key, opts.Late, lateAnswerWindow)
		}
		return clientResponse{}, clientTurnEnded, nil
	}
}

// awaitLate keeps routing alive briefly after a request was abandoned so a
// late success can release a resource; then it drops the pending entry.
func (s *Server) awaitLate(answer <-chan clientResponse, key string, late func(json.RawMessage), window time.Duration) {
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case resp := <-answer:
		if resp.err == nil && resp.result != nil {
			late(resp.result)
		}
	case <-timer.C:
	}
	s.mu.Lock()
	delete(s.pendingReq, key)
	s.mu.Unlock()
}
