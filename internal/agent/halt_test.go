package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/providers"
	"github.com/goatherdai/yaah/internal/tools"
	"github.com/goatherdai/yaah/internal/types"
)

// TestRunHalted_ClassifiesOnlyDeliberateCause pins the classification
// predicate: only a cancellation stamped with ErrRunHalted is a stop.
// Deadlines, plain cancels, and untouched contexts are not.
func TestRunHalted_ClassifiesOnlyDeliberateCause(t *testing.T) {
	caused, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunHalted)
	if !RunHalted(caused) {
		t.Error("a halt-caused cancel must classify as halted")
	}

	plain, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	if RunHalted(plain) {
		t.Error("a plain cancel must not classify as halted")
	}

	derived, cancelDerived := context.WithCancelCause(context.Background())
	child, cancelChild := context.WithCancel(derived)
	cancelDerived(ErrRunHalted)
	defer cancelChild()
	if !RunHalted(child) {
		t.Error("the halt cause must propagate through derived contexts")
	}

	live := context.Background()
	if RunHalted(live) {
		t.Error("a live context must not classify as halted")
	}
}

// haltOnFirstDelta builds a sub-agent loop against the streaming
// partial provider and returns a run context cancelled with the halt
// cause once the chunk has been observed as a token delta. A fixed sleep
// races the loop's startup — it lost on the Windows CI runner, where the
// cancel fired before the chunk was read and there was no partial to
// seal — so the mid-stream stop is gated on the delta.
func haltOnFirstDelta(t *testing.T) (*Loop, context.Context) {
	t.Helper()
	observed := make(chan struct{})
	var once sync.Once
	loop := NewSubAgentLoop(&partialThenHangProvider{}, tools.NewEmptyRegistry(), "test-model", "sys", SubAgentConfig{
		MaxLoopCycles: 1,
		MaxToolTurns:  1,
		EventObserver: func(ev Event) {
			if _, ok := ev.(*TokenDeltaEvent); ok {
				once.Do(func() { close(observed) })
			}
		},
	})

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		<-observed
		cancel(ErrRunHalted)
	}()
	t.Cleanup(func() { cancel(nil) })
	return loop, ctx
}

// TestLoop_HaltedRunSealsPartialWithMarker: a deliberate stop mid-stream
// must (1) classify the run as ErrRunHalted — not a provider or context
// error — and (2) seal the streamed partial into the conversation with
// the interrupted marker, where a deadline- or cancel-ended run leaves it
// out (see TestLoop_RunKeepsPartialContentOnDeadline).
func TestLoop_HaltedRunSealsPartialWithMarker(t *testing.T) {
	loop, ctx := haltOnFirstDelta(t)

	got, err := loop.Run(ctx, "answer me")
	if !errors.Is(err, ErrRunHalted) {
		t.Fatalf("error = %v, want ErrRunHalted", err)
	}
	if got != "almost done" {
		t.Errorf("response = %q, want the partial streamed text", got)
	}
	var sealed *string
	for i := range loop.State.Messages {
		m := loop.State.Messages[i]
		if m.Role != "assistant" {
			continue
		}
		if strings.HasPrefix(m.Content, "almost done") {
			sealed = &m.Content
		}
	}
	if sealed == nil {
		t.Fatal("the halted partial was not sealed into the conversation")
	}
	if !strings.HasSuffix(*sealed, HaltedContentSuffix) {
		t.Errorf("sealed content = %q, want the %q marker suffix", *sealed, HaltedContentSuffix)
	}
}

// TestLoop_HaltedBeforeContentIsBareHalt: a stop that lands before any
// content streamed is still classified as a halt, and seals nothing —
// there is no partial to keep.
func TestLoop_HaltedBeforeContentIsBareHalt(t *testing.T) {
	loop := NewSubAgentLoop(&hangProvider{}, tools.NewEmptyRegistry(), "test-model", "sys", SubAgentConfig{
		MaxLoopCycles: 1,
		MaxToolTurns:  1,
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel(ErrRunHalted)
	}()
	defer cancel(nil)

	got, err := loop.Run(ctx, "answer me")
	if !errors.Is(err, ErrRunHalted) {
		t.Fatalf("error = %v, want ErrRunHalted", err)
	}
	if got != "" {
		t.Errorf("response = %q, want empty", got)
	}
	for _, m := range loop.State.Messages {
		if m.Role == "assistant" {
			t.Errorf("an empty halt sealed a message: %q", m.Content)
		}
	}
}

// TestLoop_HaltedRunPersistsSealedPartial: the sealed partial must land
// in the session history — the resumed transcript and the next turn's
// context both see the interrupted answer (yaah-ny97 AC2), not only the
// in-memory conversation.
func TestLoop_HaltedRunPersistsSealedPartial(t *testing.T) {
	db := newPersistTestDB(t, "sess-halt")
	loop, ctx := haltOnFirstDelta(t)
	loop.Persister = NewSessionPersister(db, nil, "sess-halt")

	if _, err := loop.Run(ctx, "answer me"); !errors.Is(err, ErrRunHalted) {
		t.Fatalf("Run error = %v, want ErrRunHalted", err)
	}

	msgs, err := db.GetMessages("sess-halt")
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	var sealed *string
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			c := msgs[i].Content
			sealed = &c
		}
	}
	if sealed == nil {
		t.Fatal("the halted partial was never persisted")
	}
	if !strings.HasPrefix(*sealed, "almost done") || !strings.HasSuffix(*sealed, HaltedContentSuffix) {
		t.Errorf("persisted sealed content = %q, want the partial plus the %q marker", *sealed, HaltedContentSuffix)
	}
}

// hangProvider never yields a chunk: the run is stopped by the context
// alone.
type hangProvider struct{}

func (p *hangProvider) Send(context.Context, types.ChatRequest) (*types.ChatResponse, error) {
	return nil, errors.New("streaming only")
}

func (p *hangProvider) SendStream(ctx context.Context, req types.ChatRequest) (<-chan providers.StreamChunk, <-chan error) {
	return make(chan providers.StreamChunk), make(chan error)
}
