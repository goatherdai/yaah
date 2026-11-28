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

// fakeTUI records the activity calls the submit and stop paths make.
type fakeTUI struct {
	mu      sync.Mutex
	prompts int
	shows   int
	hides   int
}

func (f *fakeTUI) AddPrompt(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts++
}

func (f *fakeTUI) ShowThinking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shows++
}

func (f *fakeTUI) HideThinking() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hides++
}

func (f *fakeTUI) counts() (prompts, shows, hides int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompts, f.shows, f.hides
}

// fakeSession records what the hooks hand the session, without running
// anything. prompts is a channel because the submit hook starts RunPrompt
// on its own goroutine.
type fakeSession struct {
	prompts chan string

	mu    sync.Mutex
	halts int
}

func newFakeSession() *fakeSession {
	return &fakeSession{prompts: make(chan string, 4)}
}

func (f *fakeSession) RunPrompt(_ context.Context, prompt string) (string, bool, error) {
	f.prompts <- prompt
	return "", false, nil
}

func (f *fakeSession) HaltCurrentRun() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.halts++
}

func (f *fakeSession) haltCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.halts
}

// TestSubmitStopHooks_Contract pins the pair with nothing running: a
// submit hands the prompt to the session and shows the thinking indicator,
// and a stop hides the indicator and reaches the session's run cancel
// exactly once per invocation (yaah-l2z1).
func TestSubmitStopHooks_Contract(t *testing.T) {
	t.Run("submit hands the prompt to the session", func(t *testing.T) {
		app := &fakeTUI{}
		sess := newFakeSession()
		onSubmit, _ := submitStopHooks(app, sess)

		onSubmit("hello")

		select {
		case got := <-sess.prompts:
			if got != "hello" {
				t.Fatalf("RunPrompt prompt = %q, want %q", got, "hello")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the submit hook never started RunPrompt")
		}
		if _, shows, _ := app.counts(); shows != 1 {
			t.Fatalf("ShowThinking calls = %d, want 1", shows)
		}
	})

	t.Run("stop reaches the session once per invocation", func(t *testing.T) {
		app := &fakeTUI{}
		sess := newFakeSession()
		_, onStop := submitStopHooks(app, sess)

		onStop()
		onStop()

		if _, _, hides := app.counts(); hides != 2 {
			t.Fatalf("HideThinking calls = %d, want 2", hides)
		}
		if got := sess.haltCount(); got != 2 {
			t.Fatalf("HaltCurrentRun calls = %d, want 2", got)
		}
	})
}

// blockingProvider holds Send open until its context is cancelled, so a
// turn against it stays in flight until something cancels the run.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once

	mu    sync.Mutex
	sends int
}

func (p *blockingProvider) Send(ctx context.Context, req types.ChatRequest) (*types.ChatResponse, error) {
	p.mu.Lock()
	p.sends++
	p.mu.Unlock()
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// sendCalls reports how many turns reached the provider.
func (p *blockingProvider) sendCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sends
}

// waitForProvider blocks until a turn has entered the provider.
func waitForProvider(t *testing.T, prov *blockingProvider) {
	t.Helper()
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the provider")
	}
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

// runPublished reports whether a run has published its cancel, which is
// what HaltCurrentRun invokes.
func runPublished(s *agentSession) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runCancel != nil
}

// recordingSession is a real session whose RunPrompt returns are visible
// to a test. The submit hook starts RunPrompt on its own goroutine, so a
// queued prompt's return is otherwise unobservable.
type recordingSession struct {
	*agentSession
	returned chan error
}

func newRecordingSession(prov *blockingProvider) *recordingSession {
	return &recordingSession{
		agentSession: newWakeTurnSession(prov),
		returned:     make(chan error, 4),
	}
}

func (s *recordingSession) RunPrompt(ctx context.Context, prompt string) (string, bool, error) {
	_, _, err := s.agentSession.RunPrompt(ctx, prompt)
	s.returned <- err
	return "", false, err
}

// TestSubmitStopHooks_CancelsWakeTurn pins the bug end to end: the TUI's
// stop must reach a wake-started turn. StartWakeTurn runs wake turns on
// the session's own context without ever passing through a submit hook, so
// a cancel the TUI keeps per submit never reached them. With a real session
// running a wake turn against a provider that blocks until its context is
// cancelled, the stop must release the run gate.
func TestSubmitStopHooks_CancelsWakeTurn(t *testing.T) {
	t.Run("wake turn in flight", func(t *testing.T) {
		prov := &blockingProvider{started: make(chan struct{})}
		sess := newRecordingSession(prov)
		_, onStop := submitStopHooks(&fakeTUI{}, sess)

		if !sess.StartWakeTurn("[WATCH] ci is green") {
			t.Fatal("StartWakeTurn refused to start a turn on an idle session")
		}
		// Entering Send proves the run is in flight and its cancel published,
		// so this subtest exercises the stop against a running turn.
		waitForProvider(t, prov)

		onStop()

		if !sess.waitForRunGate(5 * time.Second) {
			t.Fatal("run gate still held after the TUI's stop — the wake turn kept running")
		}
	})

	t.Run("stop lands as soon as the turn starts", func(t *testing.T) {
		prov := &blockingProvider{started: make(chan struct{})}
		sess := newRecordingSession(prov)
		_, onStop := submitStopHooks(&fakeTUI{}, sess)

		// Deliberately no waitForProvider. StartWakeTurn has claimed the gate
		// and returned, and whether its goroutine has run yet is up to the
		// scheduler. The cancel must already be published regardless: before
		// the fix it was published from inside that goroutine, so a stop
		// issued here could be a no-op.
		if !sess.StartWakeTurn("[WATCH] ci is green") {
			t.Fatal("StartWakeTurn refused to start a turn on an idle session")
		}
		if !runPublished(sess.agentSession) {
			t.Fatal("no cancel published when StartWakeTurn returned — a stop issued now would be a no-op")
		}

		onStop()

		if !sess.waitForRunGate(5 * time.Second) {
			t.Fatal("run gate still held after the stop — the wake turn ran on")
		}
	})

	t.Run("no run in flight is a no-op", func(t *testing.T) {
		sess := newRecordingSession(&blockingProvider{started: make(chan struct{})})
		_, onStop := submitStopHooks(&fakeTUI{}, sess)

		onStop() // must not panic

		if !sess.waitForRunGate(time.Second) {
			t.Fatal("run gate unexpectedly held on a session with no run")
		}
	})
}

// TestSubmitStopHooks_StopCancelsTheQueuedPrompt pins the second thing a
// stop must reach. RunPrompt waits on the run gate, so a prompt submitted
// while a wake turn holds it has no run yet and HaltCurrentRun cannot see
// it. The stop must end the wake turn, and the queued prompt must abort as
// soon as it gets the gate without reaching the provider.
func TestSubmitStopHooks_StopCancelsTheQueuedPrompt(t *testing.T) {
	prov := &blockingProvider{started: make(chan struct{})}
	sess := newRecordingSession(prov)
	onSubmit, onStop := submitStopHooks(&fakeTUI{}, sess)

	if !sess.StartWakeTurn("[WATCH] ci is green") {
		t.Fatal("StartWakeTurn refused to start a turn on an idle session")
	}
	waitForProvider(t, prov)

	onSubmit("queued behind the wake turn")
	onStop()

	select {
	case <-sess.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued prompt is still running after the stop")
	}
	if got := prov.sendCalls(); got != 1 {
		t.Fatalf("provider Send calls = %d, want 1: the queued prompt ran despite the stop", got)
	}
	if !sess.waitForRunGate(5 * time.Second) {
		t.Fatal("run gate still held after the stop — the wake turn kept running")
	}
}
