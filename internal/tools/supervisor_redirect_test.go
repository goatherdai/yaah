package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/supervision"
	"github.com/goatherdai/yaah/internal/supervisionapi"
)

// TestSupervisorTool_ReturnsPromptlyWithoutWaitingForReplacement pins
// the redirect action's contract (yaah-8gxg.4, findings #2/#8): the
// tool claims the run's generation, deposits guidance, cancels the
// current run — and returns. It must never wait on the run's quiesce
// channel (held OPEN for this whole test, standing in for the
// replacement run the sequence still has to finish) and must never
// build a run context of its own.
func TestSupervisorTool_ReturnsPromptlyWithoutWaitingForReplacement(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	runs := supervision.NewRegistry()

	child, err := mgr.Create("sub-redir", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}
	cancelled := make(chan struct{}, 1)
	quiesce := make(chan struct{}) // stays open: a Done-wait would hang
	runs.Register(child.ID(), supervision.Run{
		SteerCh: make(chan string, supervision.SteerBuffer),
		Cancel:  func() { cancelled <- struct{}{} },
		Done:    quiesce,
	})

	sup := &SupervisorTool{ScopeManager: mgr, Runs: runs}
	args, _ := json.Marshal(map[string]string{
		"action": "redirect", "scope_id": child.ID(), "guidance": "switch to plan B"})

	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := sup.Execute(context.Background(), string(args))
		done <- outcome{out, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Execute(redirect): %v", res.err)
		}
		if !strings.Contains(res.out, "Redirect requested") {
			t.Errorf("redirect result = %s, want the prompt-request report", res.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("redirect blocked: it must return promptly, without waiting for the replacement run")
	}

	// The current run was cancelled (so its runOnce returns) — and the
	// quiesce channel is still open: nothing here waited on it.
	select {
	case <-cancelled:
	default:
		t.Error("redirect must cancel the current run before returning")
	}
	select {
	case <-quiesce:
		t.Fatal("the test's Done channel must stay open — the tool must never close or wait on it")
	default:
	}

	// The claim is single-winner: a second redirect at the same
	// generation reports the in-progress claim, not a second replacement.
	out, err := sup.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute(redirect, second): %v", err)
	}
	if !strings.Contains(out, "already claimed") {
		t.Errorf("second redirect must report the in-progress claim, got: %s", out)
	}

	// The guidance landed in the pending slot the parent loop consumes.
	gen, ok := runs.Generation(child.ID())
	if !ok {
		t.Fatal("the claimed run must stay registered until its loop adopts the handoff")
	}
	req, consumed := runs.ConsumeRedirectOrClose(child.ID(), gen)
	if !consumed {
		t.Fatal("ConsumeRedirect must return the claimed request")
	}
	if req.Guidance != "switch to plan B" {
		t.Errorf("deposited guidance = %q, want it verbatim", req.Guidance)
	}
}

// TestSupervisorTool_RedirectNoRun: a scope with no live run reports the
// refusal as a result — nothing claimed, nothing cancelled, no error.
func TestSupervisorTool_RedirectNoRun(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	runs := supervision.NewRegistry()
	idle, err := mgr.Create("sub-idle", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	sup := &SupervisorTool{ScopeManager: mgr, Runs: runs}
	args, _ := json.Marshal(map[string]string{
		"action": "redirect", "scope_id": idle.ID(), "guidance": "anything"})
	out, err := sup.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute(redirect, no run): %v", err)
	}
	if !strings.Contains(out, "No active run") {
		t.Errorf("redirect without a run must say so, got: %s", out)
	}
}

// raceNoRunBridge fakes the inspection-to-claim gap: Generation still
// reports a live generation, but Redirect finds the run gone (it
// finished, was superseded, or passed its final adoption check in
// between) and refuses with ErrNoRun.
type raceNoRunBridge struct{ gen uint64 }

func (b raceNoRunBridge) Inject(string, string) error { return supervisionapi.ErrNoRun }
func (b raceNoRunBridge) Cancel(string) (<-chan struct{}, error) {
	return nil, supervisionapi.ErrNoRun
}
func (raceNoRunBridge) ScopeIDs() []string                 { return nil }
func (b raceNoRunBridge) Generation(string) (uint64, bool) { return b.gen, true }
func (raceNoRunBridge) Redirect(string, uint64, supervisionapi.RedirectRequest) (supervisionapi.RedirectHandle, error) {
	return supervisionapi.RedirectHandle{}, supervisionapi.ErrNoRun
}
func (raceNoRunBridge) CancelGeneration(string, uint64) (<-chan struct{}, error) {
	return nil, supervisionapi.ErrNoRun
}

// TestSupervisorTool_RedirectClaimRace: a run finishing between the
// generation inspection and the claim is a normal race, not a fault
// (PR #400 review finding 1) — the tool must report the refusal as a
// result (nothing claimed; re-inspect), never as a tool error.
func TestSupervisorTool_RedirectClaimRace(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	finishing, err := mgr.Create("sub-race", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	sup := &SupervisorTool{ScopeManager: mgr, Runs: raceNoRunBridge{gen: 7}}
	for _, action := range []string{"redirect", "revert"} {
		args, _ := json.Marshal(map[string]string{
			"action": action, "scope_id": finishing.ID(), "guidance": "anything"})
		out, err := sup.Execute(context.Background(), string(args))
		if err != nil {
			t.Fatalf("Execute(%s, claim race): %v", action, err)
		}
		if !strings.Contains(out, "nothing was claimed") || !strings.Contains(out, "Re-inspect") {
			t.Errorf("%s claim-race result = %q, want a nothing-claimed, re-inspect outcome", action, out)
		}
	}
}
