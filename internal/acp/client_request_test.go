package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/jsonrpc"
)

// responseWriter decodes each outgoing request and hands it to onRequest.
// Answering synchronously inside the write exercises the pre-registration
// race without sleeps: a client that answers before sendRequest returns must
// still find its routing entry.
type responseWriter struct {
	onRequest func(jsonrpc.Message)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	var msg jsonrpc.Message
	if err := json.Unmarshal(bytes.TrimSpace(p), &msg); err == nil && msg.Method != "" && w.onRequest != nil {
		w.onRequest(msg)
	}
	return len(p), nil
}

func newRequestTestServer() *Server {
	return &Server{
		pendingReq: make(map[string]chan clientResponse),
		logf:       func(string, ...any) {},
	}
}

func requestTestConnection(s *Server, onRequest func(jsonrpc.Message)) *connection {
	return &connection{w: &responseWriter{onRequest: onRequest}, logf: s.logf}
}

func requestReply(id json.RawMessage, result string) jsonrpc.Message {
	return jsonrpc.Message{JSONRPC: "2.0", ID: id, Result: json.RawMessage(result)}
}

func requestID(t *testing.T, id json.RawMessage) int {
	t.Helper()
	var n int
	if err := json.Unmarshal(id, &n); err != nil {
		t.Fatalf("request id %s is not numeric: %v", id, err)
	}
	return n
}

func pendingCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingReq)
}

func TestRequestClient_AnswerRacingRegistration(t *testing.T) {
	s := newRequestTestServer()
	var answeredID int
	c := requestTestConnection(s, func(req jsonrpc.Message) {
		answeredID = requestID(t, req.ID)
		s.resolvePending(requestReply(req.ID, `{"ok":true}`))
	})

	resp, outcome, err := s.requestClient(context.Background(), c, "test/method", nil, clientRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != clientAnswered {
		t.Fatalf("outcome = %v, want clientAnswered", outcome)
	}
	if string(resp.result) != `{"ok":true}` {
		t.Fatalf("result = %s", resp.result)
	}
	if answeredID != 1 {
		t.Fatalf("first request id = %d, want 1", answeredID)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after answer = %d, want 0", n)
	}
}

func TestRequestClient_TimeoutCleansUp(t *testing.T) {
	s := newRequestTestServer()
	c := requestTestConnection(s, nil)

	_, outcome, err := s.requestClient(context.Background(), c, "test/method", nil,
		clientRequestOptions{Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != clientTimedOut {
		t.Fatalf("outcome = %v, want clientTimedOut", outcome)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after timeout = %d, want 0", n)
	}
}

func TestRequestClient_ContextCancellationCleansUp(t *testing.T) {
	s := newRequestTestServer()
	c := requestTestConnection(s, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, outcome, err := s.requestClient(ctx, c, "test/method", nil, clientRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != clientTurnEnded {
		t.Fatalf("outcome = %v, want clientTurnEnded", outcome)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after cancellation = %d, want 0", n)
	}
}

func TestRequestClient_LateAnswerReachesHookOnce(t *testing.T) {
	s := newRequestTestServer()
	requests := make(chan jsonrpc.Message, 1)
	c := requestTestConnection(s, func(req jsonrpc.Message) { requests <- req })
	ctx, cancel := context.WithCancel(context.Background())
	late := make(chan json.RawMessage, 1)
	outcomes := make(chan clientRequestOutcome, 1)

	go func() {
		_, outcome, err := s.requestClient(ctx, c, "terminal/create", nil,
			clientRequestOptions{Late: func(r json.RawMessage) { late <- r }})
		if err != nil {
			outcomes <- clientFailed
			return
		}
		outcomes <- outcome
	}()

	req := <-requests
	cancel()
	if outcome := <-outcomes; outcome != clientTurnEnded {
		t.Fatalf("outcome = %v, want clientTurnEnded", outcome)
	}
	// The abandoned request keeps routing alive until the late hook fires.
	s.mu.Lock()
	_, kept := s.pendingReq[permissionID(requestID(t, req.ID))]
	s.mu.Unlock()
	if !kept {
		t.Fatal("pending entry removed before the late answer could arrive")
	}

	s.resolvePending(requestReply(req.ID, `{"terminalId":"t1"}`))
	select {
	case got := <-late:
		if string(got) != `{"terminalId":"t1"}` {
			t.Fatalf("late result = %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("late hook was not called")
	}
	deadline := time.Now().Add(time.Second)
	for pendingCount(s) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after late answer = %d, want 0", n)
	}
}

func TestAwaitLate_DropsPendingOnWindowExpiry(t *testing.T) {
	s := newRequestTestServer()
	s.pendingReq["7"] = make(chan clientResponse, 1)
	called := false
	s.awaitLate(make(chan clientResponse), "7", func(json.RawMessage) { called = true }, 20*time.Millisecond)
	if called {
		t.Fatal("late hook fired without an answer")
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after late window = %d, want 0", n)
	}
}

func TestRequestClient_ErrorResponsePreservesCode(t *testing.T) {
	s := newRequestTestServer()
	c := requestTestConnection(s, func(req jsonrpc.Message) {
		s.resolvePending(jsonrpc.Message{JSONRPC: "2.0", ID: req.ID,
			Error: &RPCError{Code: -32000, jsonrpc.Message: "refused"}})
	})

	resp, outcome, err := s.requestClient(context.Background(), c, "test/method", nil, clientRequestOptions{})
	if err != nil || outcome != clientAnswered {
		t.Fatalf("requestClient: outcome=%v err=%v", outcome, err)
	}
	var clientErr *clientRequestError
	if !errors.As(resp.err, &clientErr) {
		t.Fatalf("error = %T, want *clientRequestError", resp.err)
	}
	if clientErr.Code != -32000 || clientErr.jsonrpc.Message != "refused" {
		t.Fatalf("client error = %+v", clientErr)
	}
}

func TestRequestClient_ConcurrentRequestsDoNotCrossDeliver(t *testing.T) {
	s := newRequestTestServer()
	requests := make(chan jsonrpc.Message, 2)
	c := requestTestConnection(s, func(req jsonrpc.Message) { requests <- req })

	type result struct {
		index   int
		payload string
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _, err := s.requestClient(context.Background(), c,
				fmt.Sprintf("test/%d", i), nil, clientRequestOptions{})
			results <- result{index: i, payload: string(resp.result), err: err}
		}(i)
	}

	first, second := <-requests, <-requests
	firstID, secondID := requestID(t, first.ID), requestID(t, second.ID)
	if firstID == secondID {
		t.Fatalf("both requests used id %d", firstID)
	}
	// Answer in reverse order: each waiter must get its own response.
	s.resolvePending(requestReply(second.ID, `"second"`))
	s.resolvePending(requestReply(first.ID, `"first"`))
	wg.Wait()
	close(results)

	seen := map[string]int{}
	for r := range results {
		if r.err != nil {
			t.Fatalf("request %d: %v", r.index, r.err)
		}
		seen[r.payload]++
	}
	if seen[`"first"`] != 1 || seen[`"second"`] != 1 {
		t.Fatalf("results = %v, want one first and one second", seen)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after concurrent answers = %d, want 0", n)
	}
}

func TestRequestClient_NoConnectionFailsClosed(t *testing.T) {
	s := newRequestTestServer()
	_, outcome, err := s.requestClient(context.Background(), nil, "test/method", nil, clientRequestOptions{})
	if err == nil || err.Error() != "test/method: no client connection" {
		t.Fatalf("err = %v, want no-client-connection error", err)
	}
	if outcome != clientFailed {
		t.Fatalf("outcome = %v, want clientFailed (not the zero value)", outcome)
	}
}

func TestRequestClient_NilContextWithoutTimeoutFailsClosed(t *testing.T) {
	s := newRequestTestServer()
	c := requestTestConnection(s, nil)
	var nilCtx context.Context // models a headless ask with no turn context
	_, outcome, err := s.requestClient(nilCtx, c, "test/method", nil, clientRequestOptions{})
	if err == nil || err.Error() != "test/method: nil context with no timeout" {
		t.Fatalf("err = %v, want nil-context error", err)
	}
	if outcome != clientFailed {
		t.Fatalf("outcome = %v, want clientFailed", outcome)
	}
	if n := pendingCount(s); n != 0 {
		t.Fatalf("pending entries after early failure = %d, want 0", n)
	}
}

func TestAnswerTimeout_FallbackAndOverride(t *testing.T) {
	s := newRequestTestServer()
	if got := s.answerTimeout(); got != defaultPermissionTimeout {
		t.Fatalf("answerTimeout = %v, want %v", got, defaultPermissionTimeout)
	}
	s.PermissionTimeout = 123 * time.Millisecond
	if got := s.answerTimeout(); got != 123*time.Millisecond {
		t.Fatalf("answerTimeout = %v, want 123ms", got)
	}
}
