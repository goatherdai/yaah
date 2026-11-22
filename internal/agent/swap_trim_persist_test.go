package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/goatherdai/yaah/internal/providers"
	"github.com/goatherdai/yaah/internal/types"
)

// swapTrimPrimary fails with a degenerate stream — the shape a provider whose
// stream cut before any content produces — so the client rotates to the
// fallback.
type swapTrimPrimary struct{}

func (swapTrimPrimary) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	return nil, errors.New("non-streaming response produced no content")
}

// swapTrimFallback models the swapped-to thinking-mode endpoint: it refuses any
// replayed assistant turn carrying no reasoning signature and serves everything
// else.
type swapTrimFallback struct{ messages [][]types.Message }

func (p *swapTrimFallback) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.messages = append(p.messages, req.Messages)
	for _, m := range req.Messages {
		if m.Role == "assistant" && m.ReasoningSignature == "" {
			return nil, &providers.APIError{
				StatusCode: 400,
				Message:    "The `content[].thinking` in the thinking mode must be passed back to the API.",
			}
		}
	}
	return &types.ChatResponse{Choices: []types.Choice{{
		Message:      types.Message{Role: "assistant", Content: "served", ReasoningSignature: "sig-1"},
		FinishReason: "stop",
	}}}, nil
}

// TestLoop_swapTrimRebasesPersistence pins the persistence half of the swap trim
// (yaah-1109 review): the trimmed baseline is what the loop adopts in memory, so
// the stored transcript and the cursor must be rebased to it at the same time.
// Left alone, the DB keeps the rows the trim discarded and a resumed session
// reloads the foreign history — the original 400, once per resume.
func TestLoop_swapTrimRebasesPersistence(t *testing.T) {
	const sessionID = "sess-swap-trim"
	db := newPersistTestDB(t, sessionID)
	history := []types.Message{
		types.SystemMsg("system prompt"),
		types.UserMsg("first ask"),
		{Role: "assistant", Content: "old answer"}, // unsigned: the primary was not a thinking provider
		types.UserMsg("second ask"),
		{Role: "assistant", Content: "older answer"},
		types.UserMsg("current ask"),
	}
	fallback := &swapTrimFallback{}
	loop := &Loop{
		Config:           LoopConfig{SessionID: sessionID, Model: "primary-model", MaxRetries: 2},
		Persister:        NewSessionPersister(db, nil, sessionID),
		Provider:         swapTrimPrimary{},
		FallbackProvider: fallback,
		State:            LoopState{Messages: history},
	}
	loop.applyDefaults()
	if loop.LLM == nil || loop.LLM.OnMessagesReplaced == nil || loop.LLM.CanonicalMessages == nil {
		t.Fatal("applyDefaults did not wire the swap-trim seams onto the LLM client")
	}

	for _, m := range history {
		loop.Persister.Persist(m)
	}
	if got := loop.Persister.MsgIdx(); got != len(history) {
		t.Fatalf("MsgIdx after persisting the history = %d, want %d", got, len(history))
	}

	res, err := loop.LLM.Call(context.Background(), types.ChatRequest{Model: "primary-model", Messages: history})
	if err != nil {
		t.Fatalf("Call() error after the swap trim: %v", err)
	}
	if res.Message.Content != "served" {
		t.Fatalf("content = %q, want %q", res.Message.Content, "served")
	}

	// The adopted baseline is [system, current ask]: the foreign turns must be
	// gone from the DB as well as from memory.
	rows, err := db.GetMessages(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"system prompt", "current ask"}
	if len(rows) != len(want) {
		t.Fatalf("persisted rows = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Idx != i || rows[i].Content != w {
			t.Errorf("row %d = idx=%d content=%q; want idx=%d %q", i, rows[i].Idx, rows[i].Content, i, w)
		}
	}
	if got := loop.Persister.MsgIdx(); got != len(want) {
		t.Errorf("MsgIdx after the swap trim = %d, want %d (the cursor must follow the shortened transcript)", got, len(want))
	}

	// A resumed session appends past the rebased baseline instead of colliding
	// with rows the trim discarded.
	loop.Persister.Persist(types.AssistantMsg("served", nil))
	rows, err = db.GetMessages(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows after the post-trim persist = %d, want 3", len(rows))
	}
	if rows[2].Idx != 2 || rows[2].Content != "served" {
		t.Errorf("post-trim row = idx=%d content=%q; want idx=2 %q", rows[2].Idx, rows[2].Content, "served")
	}
}
