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
)

// TestSupervisorTool_RevertActsLikeRedirectWithRevertFlag pins
// yaah-1050.1.3's LLM-side contract: the `revert` action is the
// redirect action's shared helper with Revert=true — it claims the
// run's generation, deposits RedirectRequest{Guidance, Revert:true},
// cancels the current run, and returns promptly without waiting on the
// (deliberately still-open) quiesce channel. ConsumeRedirect must hand
// the adoption loop both the guidance and the rollback mark.
func TestSupervisorTool_RevertActsLikeRedirectWithRevertFlag(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	runs := supervision.NewRegistry()

	child, err := mgr.Create("sub-revert", nil)
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
		"action": "revert", "scope_id": child.ID(), "guidance": "restart from the clean state"})

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
			t.Fatalf("Execute(revert): %v", res.err)
		}
		if !strings.Contains(res.out, "Revert requested") {
			t.Errorf("revert result = %s, want the revert-request report", res.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revert blocked: it must return promptly, exactly like redirect")
	}

	// Same LLM-side effect as redirect: the current run was cancelled
	// and the quiesce channel was never waited on.
	select {
	case <-cancelled:
	default:
		t.Error("revert must cancel the current run before returning")
	}
	select {
	case <-quiesce:
		t.Fatal("the test's Done channel must stay open — revert must never close or wait on it")
	default:
	}

	// The deposit carries the rollback mark for the adoption loop.
	gen, ok := runs.Generation(child.ID())
	if !ok {
		t.Fatal("the claimed run must stay registered until its loop adopts the handoff")
	}
	req, consumed := runs.ConsumeRedirect(child.ID(), gen)
	if !consumed {
		t.Fatal("ConsumeRedirect must return the claimed request")
	}
	if req.Guidance != "restart from the clean state" {
		t.Errorf("deposited guidance = %q, want it verbatim", req.Guidance)
	}
	if !req.Revert {
		t.Error("revert must deposit Revert=true so the adoption loop rolls the workspace back")
	}

	// Same single-winner refusal as redirect: a second revert reports
	// the in-progress claim, never a second replacement.
	out, err := sup.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute(revert, second): %v", err)
	}
	if !strings.Contains(out, "already claimed") {
		t.Errorf("second revert must report the in-progress claim, got: %s", out)
	}
}

// TestSupervisorTool_RevertNoRun mirrors redirect's refusal: a scope
// with no live run reports the refusal as a result — nothing claimed,
// nothing cancelled, no error.
func TestSupervisorTool_RevertNoRun(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)
	runs := supervision.NewRegistry()
	idle, err := mgr.Create("sub-idle-revert", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	sup := &SupervisorTool{ScopeManager: mgr, Runs: runs}
	args, _ := json.Marshal(map[string]string{
		"action": "revert", "scope_id": idle.ID(), "guidance": "anything"})
	out, err := sup.Execute(context.Background(), string(args))
	if err != nil {
		t.Fatalf("Execute(revert, no run): %v", err)
	}
	if !strings.Contains(out, "No active run") {
		t.Errorf("revert without a run must say so, got: %s", out)
	}
}
