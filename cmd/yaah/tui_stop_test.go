package yaah

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/agent"
	"github.com/goatherdai/yaah/internal/config"
	"github.com/goatherdai/yaah/internal/types"
)

// fakeStopUI records HideThinking calls from the stop path.
type fakeStopUI struct {
	mu    sync.Mutex
	hides int
}

func (f *fakeStopUI) HideThinking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hides++
}

func (f *fakeStopUI) hideCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hides
}

// fakeRunCanceller records HaltCurrentRun calls from the stop path.
type fakeRunCanceller struct {
	mu     sync.Mutex
	halted int
}

func (f *fakeRunCanceller) HaltCurrentRun() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.halted++
}

func (f *fakeRunCanceller) haltCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.halted
}

// TestStopRun_RoutesThroughTheSession pins the stop path's contract: every
// invocation of the hook stopRun returns hides the thinking indicator and
// halts the session's run exactly once (yaah-l2z1).
func TestStopRun_RoutesThroughTheSession(t *testing.T) {
	app := &fakeStopUI{}
	sess := &fakeRunCanceller{}
	stop := stopRun(app, sess)

	stop()
	stop()

	if got := app.hideCount(); got != 2 {
		t.Fatalf("HideThinking calls = %d, want 2", got)
	}
	if got := sess.haltCount(); got != 2 {
		t.Fatalf("HaltCurrentRun calls = %d, want 2", got)
	}
}

// blockingProvider holds Send open until its context is cancelled, so a
// turn against it stays in flight until something cancels the run.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// newWakeTurnSession builds a real agentSession that can run a prompt
// against provider, following the hand-built shape the package's other
// session tests use (session_serialize_test.go, wake_session_test.go):
// a zero-value session plus the fields a run needs. Persistence is nil
// (disabled) and the view nil (NoopView), so the run exercises the loop,
// the run gate and nothing else.
func newWakeTurnSession(provider agent.Provider) *agentSession {
	return &agentSession{
		cfg:        &config.Config{},
		provider:   provider,
		steerCh:    make(chan string, 8),
		followupCh: make(chan string, 8),
	}
}

// TestStopRun_CancelsWakeTurn pins the bug end to end: the TUI's stop must
// cancel the session's run, not a driver-local one. StartWakeTurn runs wake
// turns on the session's own context without ever passing through a submit
// hook, so the old TUI-local cancel never reached them. With a real session
// running a wake turn against a provider that blocks until its context is
// cancelled, the stop must release the run gate. Reverting stopRun to a
// no-op leaves the gate held and fails the first subtest.
func TestStopRun_CancelsWakeTurn(t *testing.T) {
	t.Run("wake turn is cancelled", func(t *testing.T) {
		prov := &blockingProvider{started: make(chan struct{})}
		sess := newWakeTurnSession(prov)

		if !sess.StartWakeTurn("[WATCH] ci is green") {
			t.Fatal("StartWakeTurn refused to start a turn on an idle session")
		}

		// started closing is the in-flight signal: Send can only be entered
		// after runPromptLocked published s.runCancel under s.mu, so a stop
		// from here lands on the running turn.
		select {
		case <-prov.started:
		case <-time.After(5 * time.Second):
			t.Fatal("wake turn never reached the provider")
		}

		stopRun(&fakeStopUI{}, sess)()

		if !sess.waitForRunGate(5 * time.Second) {
			t.Fatal("run gate still held after the TUI's stop — the wake turn kept running")
		}
	})

	t.Run("no run in flight is a no-op", func(t *testing.T) {
		sess := newWakeTurnSession(&blockingProvider{started: make(chan struct{})})

		stopRun(&fakeStopUI{}, sess)() // must not panic

		if !sess.waitForRunGate(time.Second) {
			t.Fatal("run gate unexpectedly held on a session with no run")
		}
	})
}
