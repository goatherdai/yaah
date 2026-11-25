package yaah

// session_supervise_teardown_test.go pins the ONE ordering in teardown that is
// invisible from either component's own behavior (yaah-fy28, review r3 finding
// 5): the session releases the supervise tool's arbiter routing BEFORE it closes
// the advisory rules service.
//
// The order matters because the two are the same arbiter. While a batch is bound
// the rules service's firings queue into that batch instead of being delivered,
// so closing the service with the routing still armed lets the drain's last
// firings land in a queue nothing will ever tick — they drop past the batch's cap
// and the session's rules surface degrades silently. Releasing first puts the
// engine back on direct delivery for whatever Close still drains.
//
// The test runs the REAL teardown (close() -> shutdown()) on a minimal session:
// a registry holding an armed supervise tool, and a rules service that records
// its Close in a shared ordered log. Deleting the release block, moving it after
// the service close, or regressing the type switch (the checked-assertion form
// panics on a missing tool) each fails here.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/goatherdai/yaah/internal/supervisionapi"
	"github.com/goatherdai/yaah/internal/tools"
)

// teardownLog is the ordered event log the assertions read: one entry per
// observable teardown act, in the order the acts happened.
type teardownLog struct {
	mu     sync.Mutex
	events []string
}

func (l *teardownLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *teardownLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *teardownLog) count(event string) int {
	n := 0
	for _, e := range l.snapshot() {
		if e == event {
			n++
		}
	}
	return n
}

// recordingRulesService is the rulesService the session's teardown closes. Its
// only job is to be observable: the event log entry it appends is what the
// ordering assertion compares against the supervise release.
type recordingRulesService struct {
	log *teardownLog
}

func (r *recordingRulesService) Close() { r.log.add("rules-close") }

// teardownBatch is the tool's SuperviseBatch: one tick that leaves a worker
// live, so the call that binds the batch does not report Done and the arbiter
// stays armed into teardown — the state the ordering is about.
type teardownBatch struct{}

func (teardownBatch) Next(context.Context, []supervisionapi.Decision) (supervisionapi.TickResult, error) {
	return supervisionapi.TickResult{Done: false}, nil
}

func (teardownBatch) SnapshotText(int) string { return "w1 (scope:sub-a): running" }

// armedSuperviseTool builds the tool the composition root registers but with the
// binding hooks recording into the shared log, then calls it once so the arbiter
// is armed exactly as a live supervise loop leaves it.
func armedSuperviseTool(t *testing.T, log *teardownLog) *tools.SuperviseTool {
	t.Helper()
	tool := &tools.SuperviseTool{
		CallerScopeID: "scope:root",
		NewBatch: func(string) *tools.SuperviseBinding {
			return &tools.SuperviseBinding{
				Batch:   teardownBatch{},
				Arm:     func() { log.add("arm") },
				Release: func() { log.add("release") },
			}
		},
	}
	if _, err := tool.Execute(context.Background(), "{}"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := log.snapshot(); len(got) != 1 || got[0] != "arm" {
		t.Fatalf("events after one call = %v, want exactly the arming (a live batch keeps the rules engine routed into it)", got)
	}
	return tool
}

// foreignTool is a tool registered under the supervise name that is NOT a
// *tools.SuperviseTool — the wrapper/rename case the teardown's type switch
// must survive without releasing anything.
type foreignTool struct{}

func (foreignTool) Name() string            { return "supervise" }
func (foreignTool) Description() string     { return "a stand-in registered under the supervise name" }
func (foreignTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (foreignTool) Execute(context.Context, string) (string, error) { return "", nil }

// TestSessionTeardownReleasesSuperviseBeforeRulesClose pins the teardown
// ordering: release (exactly once) BEFORE the rules service's Close, for a
// session whose supervise tool is armed; nothing at all for a session with no
// supervise tool; and nothing (and no panic) when the name is taken by a tool of
// another type.
func TestSessionTeardownReleasesSuperviseBeforeRulesClose(t *testing.T) {
	t.Run("armed tool", func(t *testing.T) {
		log := &teardownLog{}
		reg := tools.NewEmptyRegistry()
		reg.Register(armedSuperviseTool(t, log))

		sess := &agentSession{toolReg: reg, supervisor: &recordingRulesService{log: log}}
		sess.close()

		if got, want := log.snapshot(), []string{"arm", "release", "rules-close"}; !equalEvents(got, want) {
			t.Fatalf("teardown events = %v, want %v: the arbiter must be released exactly once and BEFORE the rules service closes", got, want)
		}
		if n := log.count("release"); n != 1 {
			t.Errorf("release ran %d time(s), want 1", n)
		}
	})

	t.Run("no supervise tool", func(t *testing.T) {
		log := &teardownLog{}
		sess := &agentSession{toolReg: tools.NewEmptyRegistry(), supervisor: &recordingRulesService{log: log}}
		sess.close()

		if got, want := log.snapshot(), []string{"rules-close"}; !equalEvents(got, want) {
			t.Fatalf("teardown events = %v, want %v: a session with no supervise tool has nothing to release, and must not panic on the nil entry", got, want)
		}
	})

	t.Run("unexpected type under the name", func(t *testing.T) {
		log := &teardownLog{}
		reg := tools.NewEmptyRegistry()
		reg.Register(foreignTool{})

		sess := &agentSession{toolReg: reg, supervisor: &recordingRulesService{log: log}}
		sess.close()

		if got, want := log.snapshot(), []string{"rules-close"}; !equalEvents(got, want) {
			t.Fatalf("teardown events = %v, want %v: an unexpected registry entry is skipped (warned), never released and never a panic", got, want)
		}
		if n := log.count("release"); n != 0 {
			t.Errorf("release ran %d time(s), want 0: nothing of another type owns an arbiter hook", n)
		}
	})
}

func equalEvents(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
