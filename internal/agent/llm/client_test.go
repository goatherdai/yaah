package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/prompts"
	"github.com/goatherdai/yaah/internal/types"
)

// stripReasoningProvider always fails with the error message that
// classifies as ShouldStripReasoning, simulating a provider that
// persistently rejects reasoning content no matter how often it is
// stripped from history.
type stripReasoningProvider struct {
	calls int
}

func (p *stripReasoningProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.calls++
	return nil, errors.New(
		"invalid_request_error: reasoning_content is not supported by this model [msgs=3 roles=user]")
}

// swapStateProvider models the fallback: after the swap it always fails
// with the thinking-replay 400 (a thinking-mode endpoint that cannot
// consume the swapped-out primary's history).
type swapStateProvider struct{ calls int }

func (p *swapStateProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.calls++
	return nil, statusError{
		msg:    "The `content[].thinking` in the thinking mode must be passed back to the API.",
		status: 400,
	}
}

// degeneratePrimaryProvider fails with a degenerate stream until it has
// been swapped away from (mirroring a provider whose stream cut before any
// content).
type degeneratePrimaryProvider struct{ calls int }

func (p *degeneratePrimaryProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.calls++
	return nil, errors.New("non-streaming response produced no content")
}

// TestCall_swapStatePersistsAcrossTurns pins the yaah-31j wiring: the
// swap happened in Call 1, but the mixed history keeps failing on the
// thinking-mode fallback in Call 2 and beyond — so the swap attribution
// must persist on the client, not in per-Call state.
func TestCall_swapStatePersistsAcrossTurns(t *testing.T) {
	primary := &degeneratePrimaryProvider{}
	fallback := &swapStateProvider{}
	c := &Client{
		Provider:         primary,
		FallbackProvider: fallback,
		Model:            "test-model",
		MaxRetries:       2,
		RetryBackoff:     time.Millisecond,
		StripReasoning:   func() {},
	}

	for turn := 1; turn <= 2; turn++ {
		_, err := c.Call(context.Background(), types.ChatRequest{Model: "test-model"})
		if err == nil {
			t.Fatalf("turn %d: expected the thinking-replay error to surface", turn)
		}
		if !strings.Contains(err.Error(), "swapped-out primary") {
			t.Fatalf("turn %d: error must attribute the failure to the mid-session swap, got: %s", turn, err.Error())
		}
		if strings.Contains(err.Error(), "set `thinking: true`") {
			t.Fatalf("turn %d: config advice must not be given after a swap, got: %s", turn, err.Error())
		}
	}
}

// TestCall_swapStatePersistsAcrossCredentialRotations exercises the
// credential-rotation swap path specifically: a typed 401 from the primary
// triggers ShouldRotateCred → swapToFallback, and the attribution must
// persist on the client exactly as it does for the degenerate-stream path
// (yaah-31j review: reverting the flag on this path must fail this test).
func TestCall_swapStatePersistsAcrossCredentialRotations(t *testing.T) {
	primary := &healingProvider{
		failErr:  statusError{msg: "request failed with status 401: invalid api key", status: 401},
		maxFails: 1000, // never heals
	}
	fallback := &swapStateProvider{}
	c := &Client{
		Provider:         primary,
		FallbackProvider: fallback,
		Model:            "primary-model",
		FallbackModel:    "fallback-model",
		MaxRetries:       2,
		RetryBackoff:     time.Millisecond,
	}

	for turn := 1; turn <= 2; turn++ {
		_, err := c.Call(context.Background(), types.ChatRequest{Model: "primary-model"})
		if err == nil {
			t.Fatalf("turn %d: expected the thinking-replay error to surface", turn)
		}
		if !strings.Contains(err.Error(), "swapped-out primary") {
			t.Fatalf("turn %d: error must attribute the failure to the credential-rotation swap, got: %s", turn, err.Error())
		}
	}
}

// TestCall_Auth401FailsFastWithoutFallback pins yaah-gkll.1: with no
// fallback configured, a 401 must fail the call on the FIRST attempt — the
// identical request cannot authenticate on replay, so the backoff ladder is
// pure wall-clock burn (~65s per dispatch in the PR #1573 run).
func TestCall_Auth401FailsFastWithoutFallback(t *testing.T) {
	p := &healingProvider{
		failErr:  statusError{msg: "request failed with status 401: invalid api key", status: 401},
		maxFails: 1000, // never heals
	}
	c := &Client{
		Provider:     p,
		Model:        "test-model",
		MaxRetries:   3,
		RetryBackoff: time.Millisecond,
	}

	_, err := c.Call(context.Background(), types.ChatRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("expected the 401 to surface")
	}
	// healingProvider.stripped counts failed Sends; one failure means the
	// ladder never ran.
	if p.stripped != 1 {
		t.Fatalf("provider called %d times — a 401 must fail on the first attempt, not walk the backoff ladder", p.stripped)
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("error must carry the classification guidance, got: %s", err)
	}
}

func TestCall_stripReasoningRetryIsBounded(t *testing.T) {
	p := &stripReasoningProvider{}
	c := &Client{
		Provider:       p,
		Model:          "test-model",
		MaxRetries:     2,
		RetryBackoff:   time.Millisecond,
		StripReasoning: func() {},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Call(ctx, types.ChatRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("expected error after bounded retries")
	}

	// Strip attempts (capped at 3) do not advance the attempt counter;
	// everything else is bounded by MaxRetries. Total provider calls must
	// stay within strip cap + retries + margin — never unbounded.
	maxCalls := 3 + 2 + 2
	if p.calls > maxCalls {
		t.Fatalf("provider called %d times — strip-reasoning retry is unbounded (A2), want <= %d", p.calls, maxCalls)
	}
}

func TestCall_stripReasoningRecoversWhenProviderHeals(t *testing.T) {
	p := &healingProvider{
		failErr: errors.New("invalid_request_error: reasoning_content is not supported by this model [msgs=3 roles=user]"),
		response: &types.ChatResponse{
			Choices: []types.Choice{{
				Message:      types.Message{Role: "assistant", Content: "recovered"},
				FinishReason: "stop",
			}},
		},
		maxFails: 1,
	}
	c := &Client{
		Provider:       p,
		Model:          "test-model",
		MaxRetries:     3,
		RetryBackoff:   time.Millisecond,
		StripReasoning: func() {},
	}

	res, err := c.Call(context.Background(), types.ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("Call() error after strip recovery: %v", err)
	}
	if res.Message.Content != "recovered" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "recovered")
	}
	if p.stripped == 0 {
		t.Error("expected at least one strip attempt before recovery")
	}
}

// statusError carries a typed HTTP status so Classify can read it, mirroring
// providers.APIError.
type statusError struct {
	msg    string
	status int
}

func (e statusError) Error() string        { return e.msg }
func (e statusError) HTTPStatus() int      { return e.status }
func (e statusError) ProviderCode() string { return "" }

// Mistral-hosted GLM rejects a replayed reasoning_content with 422
// extra_forbidden. That used to abort because the strip classification only
// ran for 400 and unknown status. The client must strip and replay on 422 too.
func TestCall_stripReasoningRecoversFrom422(t *testing.T) {
	p := &healingProvider{
		failErr: statusError{
			status: 422,
			msg:    `provider returned 422: {"detail":[{"type":"extra_forbidden","loc":["body","messages",2,"assistant","reasoning_content"],"msg":"Extra inputs are not permitted"}]}`,
		},
		response: &types.ChatResponse{
			Choices: []types.Choice{{
				Message:      types.Message{Role: "assistant", Content: "recovered"},
				FinishReason: "stop",
			}},
		},
		maxFails: 1,
	}
	strips := 0
	c := &Client{
		Provider:       p,
		Model:          "test-model",
		MaxRetries:     3,
		RetryBackoff:   time.Millisecond,
		StripReasoning: func() { strips++ },
	}

	res, err := c.Call(context.Background(), types.ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("Call() error after 422 strip recovery: %v", err)
	}
	if res.Message.Content != "recovered" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "recovered")
	}
	if strips == 0 {
		t.Error("expected a strip attempt before recovering from the 422")
	}
}

// healingProvider fails with failErr while stripped < maxFails, then
// succeeds with the canned response. stripped counts the failures.
type healingProvider struct {
	failErr  error
	response *types.ChatResponse
	maxFails int
	stripped int
}

func (p *healingProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	if p.stripped < p.maxFails {
		p.stripped++
		return nil, p.failErr
	}
	return p.response, nil
}

// A provider in thinking mode demands reasoning_content be replayed. Stripping
// is the OPPOSITE of the remedy: it guarantees the identical failure forever.
// The client must classify it as config-actionable and abort, not strip-retry.
func TestCall_thinkingRequiredAbortsWithoutStripping(t *testing.T) {
	p := &healingProvider{
		failErr:  errors.New("invalid_request_error: The `content[].thinking` in the thinking mode must be passed back to the API."),
		maxFails: 100,
	}
	strips := 0
	c := &Client{
		Provider:       p,
		Model:          "test-model",
		MaxRetries:     3,
		RetryBackoff:   time.Millisecond,
		StripReasoning: func() { strips++ },
	}

	_, err := c.Call(context.Background(), types.ChatRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("expected a non-retryable error")
	}
	if strips != 0 {
		t.Errorf("StripReasoning invoked %d times — stripping cannot satisfy a provider demanding the content back", strips)
	}
	if p.stripped != 1 {
		t.Errorf("provider called %d times, want 1 (non-retryable abort)", p.stripped)
	}
}

// swapThinkingModeProvider is a thinking-mode endpoint: it rejects any request
// replaying an assistant turn without a reasoning signature — the shape a
// swapped-out non-thinking primary produces — and serves everything else.
// messages records every payload it was handed, so a test can assert what was
// actually replayed.
type swapThinkingModeProvider struct {
	messages [][]types.Message
}

func (p *swapThinkingModeProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.messages = append(p.messages, req.Messages)
	for _, m := range req.Messages {
		if m.Role == "assistant" && m.ReasoningSignature == "" {
			return nil, statusError{
				msg:    "The `content[].thinking` in the thinking mode must be passed back to the API.",
				status: 400,
			}
		}
	}
	return &types.ChatResponse{Choices: []types.Choice{{
		Message:      types.Message{Role: "assistant", Content: "served", ReasoningSignature: "sig-1"},
		FinishReason: "stop",
	}}}, nil
}

// TestCall_swapTrimsForeignThinkinglessHistory pins yaah-1109: after a swap onto
// a thinking-mode provider, the replayed history produced by the non-thinking
// primary carries no reasoning signature, so the endpoint answers 400 demanding
// the thinking blocks back. Neither stripping nor a config change applies — the
// history itself is the problem — so the client trims it back to the current
// user turn and replays, reporting the trimmed baseline for the loop to adopt.
func TestCall_swapTrimsForeignThinkinglessHistory(t *testing.T) {
	primary := &degeneratePrimaryProvider{}
	fallback := &swapThinkingModeProvider{}
	history := []types.Message{
		types.SystemMsg("system prompt"),
		types.UserMsg("first ask"),
		{Role: "assistant", Content: "old answer"}, // no signature: the primary was not a thinking provider
		types.UserMsg("second ask"),
		{Role: "assistant", Content: "older answer"},
		types.UserMsg("current ask"),
	}
	c := &Client{
		Provider:         primary,
		FallbackProvider: fallback,
		Model:            "primary-model",
		FallbackModel:    "thinking-model",
		MaxRetries:       2,
		RetryBackoff:     time.Millisecond,
	}

	res, err := c.Call(context.Background(), types.ChatRequest{Model: "primary-model", Messages: history})
	if err != nil {
		t.Fatalf("Call() error after the swap trim: %v", err)
	}
	if res.Message.Content != "served" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "served")
	}
	if !res.Compacted {
		t.Error("the trimmed baseline must be reported as Compacted so the loop adopts it")
	}
	want := []types.Message{types.SystemMsg("system prompt"), types.UserMsg("current ask")}
	if len(res.CompactedMessages) != len(want) {
		t.Fatalf("CompactedMessages = %+v, want %+v", res.CompactedMessages, want)
	}
	for i := range want {
		if res.CompactedMessages[i].Content != want[i].Content {
			t.Errorf("CompactedMessages[%d].Content = %q, want %q", i, res.CompactedMessages[i].Content, want[i].Content)
		}
	}
	if len(fallback.messages) != 2 {
		t.Fatalf("fallback attempts = %d, want 2 (foreign replay, then trimmed replay)", len(fallback.messages))
	}
	if len(fallback.messages[0]) != len(history) {
		t.Errorf("first fallback attempt replayed %d messages, want the full %d", len(fallback.messages[0]), len(history))
	}
	if len(fallback.messages[1]) != len(want) {
		t.Errorf("replay carried %d messages, want the trimmed %d", len(fallback.messages[1]), len(want))
	}
}

// TestCall_swapTrimKeepsRequestUnderTrailingNotice pins the request-preservation
// half of the swap trim: buildTurnRequest appends a transient wrap-up/budget
// notice AFTER the user turn, and that notice is itself a user-role message — so
// trimming to the final user message keeps [system, notice] and drops the
// operator's actual request. The boundary must come from canonical history, and
// the adopted baseline must reach the persistence rebase seam.
func TestCall_swapTrimKeepsRequestUnderTrailingNotice(t *testing.T) {
	notice := prompts.WrapUpMessage(1)
	canonical := []types.Message{
		types.SystemMsg("system prompt"),
		types.UserMsg("first ask"),
		{Role: "assistant", Content: "old answer"},
		types.UserMsg("current ask"),
	}
	request := append(append([]types.Message{}, canonical...), types.UserMsg(notice))

	primary := &degeneratePrimaryProvider{}
	fallback := &swapThinkingModeProvider{}
	var replaced []types.Message
	c := &Client{
		Provider:           primary,
		FallbackProvider:   fallback,
		Model:              "primary-model",
		FallbackModel:      "thinking-model",
		MaxRetries:         2,
		RetryBackoff:       time.Millisecond,
		CanonicalMessages:  func() []types.Message { return canonical },
		OnMessagesReplaced: func(msgs []types.Message) { replaced = msgs },
	}

	res, err := c.Call(context.Background(), types.ChatRequest{Model: "primary-model", Messages: request})
	if err != nil {
		t.Fatalf("Call() error after the swap trim: %v", err)
	}
	want := []types.Message{
		types.SystemMsg("system prompt"),
		types.UserMsg("current ask"),
		types.UserMsg(notice),
	}
	if !res.Compacted {
		t.Fatal("the trimmed baseline must be reported as Compacted so the loop adopts it")
	}
	if len(res.CompactedMessages) != len(want) {
		t.Fatalf("CompactedMessages = %+v, want the request turn plus its trailing notice", res.CompactedMessages)
	}
	for i := range want {
		if res.CompactedMessages[i].Content != want[i].Content {
			t.Errorf("CompactedMessages[%d].Content = %q, want %q", i, res.CompactedMessages[i].Content, want[i].Content)
		}
	}
	if len(replaced) != len(want) {
		t.Fatalf("persistence rebase received %d messages; want the trimmed %d", len(replaced), len(want))
	}
	for i := range want {
		if replaced[i].Content != want[i].Content {
			t.Errorf("rebase message %d = %q, want %q", i, replaced[i].Content, want[i].Content)
		}
	}
	if len(fallback.messages) != 2 {
		t.Fatalf("fallback attempts = %d, want 2 (foreign replay, then trimmed replay)", len(fallback.messages))
	}
}

// TestCall_midToolLoopSwapSkipsTrim: the trim keeps everything from the current
// user message onward, so a swap that fires mid-tool-loop still carries the
// turn's own unsigned assistant tool-call turns and the endpoint refuses the
// trimmed payload for the same reason. The client must not adopt that
// still-broken baseline — no trim, no persistence rebase — and must surface the
// swap diagnosis instead.
func TestCall_midToolLoopSwapSkipsTrim(t *testing.T) {
	call := types.ToolCall{ID: "c1", Function: types.ToolCallFn{Name: "read", Arguments: "{}"}}
	history := []types.Message{
		types.SystemMsg("system prompt"),
		types.UserMsg("first ask"),
		{Role: "assistant", Content: "old answer"},
		types.UserMsg("current ask"),
		{Role: "assistant", ToolCalls: []types.ToolCall{call}},
		{Role: "tool", Name: "read", ToolCallID: "c1", Content: "result"},
	}
	primary := &degeneratePrimaryProvider{}
	fallback := &swapThinkingModeProvider{}
	rebased := false
	c := &Client{
		Provider:           primary,
		FallbackProvider:   fallback,
		Model:              "primary-model",
		FallbackModel:      "thinking-model",
		MaxRetries:         2,
		RetryBackoff:       time.Millisecond,
		OnMessagesReplaced: func([]types.Message) { rebased = true },
	}

	res, err := c.Call(context.Background(), types.ChatRequest{Model: "primary-model", Messages: history})
	if err == nil {
		t.Fatal("expected the thinking-replay error to surface")
	}
	if !strings.Contains(err.Error(), "swapped-out primary") {
		t.Fatalf("error must attribute the failure to the mid-session swap, got: %s", err.Error())
	}
	if res.Compacted {
		t.Error("a trimmed history that still carries unsigned assistant turns must not be adopted as the baseline")
	}
	if rebased {
		t.Error("persistence must not be rebased onto a history the provider still rejects")
	}
	if len(fallback.messages) != 1 {
		t.Errorf("fallback attempts = %d, want 1 (the trimmed replay is refused, so it is never sent)", len(fallback.messages))
	}
}

// TestCall_swapWithoutTrimmableHistoryKeepsTheSwapDiagnosis: the trim is only a
// recovery when there is foreign history to drop. With nothing to trim (no
// assistant turns) the swap cannot be rescued, and the error must still name the
// swap instead of handing out the config advice.
func TestCall_swapWithoutTrimmableHistoryKeepsTheSwapDiagnosis(t *testing.T) {
	primary := &degeneratePrimaryProvider{}
	fallback := &swapStateProvider{}
	c := &Client{
		Provider:         primary,
		FallbackProvider: fallback,
		Model:            "primary-model",
		FallbackModel:    "thinking-model",
		MaxRetries:       2,
		RetryBackoff:     time.Millisecond,
	}

	_, err := c.Call(context.Background(), types.ChatRequest{
		Model:    "primary-model",
		Messages: []types.Message{types.SystemMsg("system prompt"), types.UserMsg("current ask")},
	})
	if err == nil {
		t.Fatal("expected the thinking-replay error to surface")
	}
	if !strings.Contains(err.Error(), "swapped-out primary") {
		t.Fatalf("error must attribute the failure to the mid-session swap, got: %s", err.Error())
	}
	if strings.Contains(err.Error(), "set `thinking: true`") {
		t.Fatalf("config advice must not be given after a swap, got: %s", err.Error())
	}
}
