package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kerneltrace "github.com/goatherdai/goatherd-kernel"
	goatherd "github.com/goatherdai/goatherd-runtime"

	"github.com/goatherdai/yaah/internal/agent/supervision"
)

// TestSupervisorStatusWithTypedNilService pins the wiring case where tracing
// is active but the advisory service is off: a nil *supervision.Service in
// the Svc interface must read as absent, not panic on Firings().
func TestSupervisorStatusWithTypedNilService(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	mgr := goatherd.NewScopeManager(store)

	tool := &SupervisorTool{
		ScopeManager: mgr,
		Svc:          (*supervision.Service)(nil),
	}
	out, err := tool.Execute(context.Background(), `{"action":"status"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "supervision service not active") {
		t.Errorf("status with a disabled service = %q, want the inactive report", out)
	}
}

// TestSupervisorTool_StatusListsFirings pins the B4 surface end to end:
// a rule firing this session shows up in `supervisor status` — rule,
// scope, count, kind — once the firing has been delivered.
func TestSupervisorTool_StatusListsFirings(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	bus := kerneltrace.NewEffectBus(64)
	t.Cleanup(bus.Close)
	store.WithBus(bus)

	mgr := goatherd.NewScopeManager(store)

	root, err := mgr.Create("sess-status", nil)
	if err != nil {
		t.Fatalf("create root scope: %v", err)
	}
	child, err := mgr.Create("sub-status", nil)
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	runs := supervision.NewRegistry()
	steer := make(chan string, supervision.SteerBuffer)
	runs.Register(child.ID(), supervision.Run{SteerCh: steer})

	svc := supervision.NewService(mgr, bus, runs,
		supervision.AdvisoryRules{DestructiveTool: true}, false, false, root.ID())
	svc.Start("status-test")
	t.Cleanup(svc.Close)

	bus.Publish(kerneltrace.EffectEvent{
		TraceOwnerID: "sub-status",
		Mode:         kerneltrace.Declaration,
		SchemaRef:    "yaah.tool.bash.v1",
		KindLabel:    "bash",
		Payload:      map[string]any{"cmd": "rm -rf /tmp/probe"},
		Timestamp:    time.Now(),
	})
	select {
	case <-steer:
	case <-time.After(2 * time.Second):
		t.Fatal("the rule firing never reached the steering channel")
	}

	sup := &SupervisorTool{RootScopeID: root.ID(), ScopeManager: mgr, Runs: runs, Svc: svc}
	out, err := sup.Execute(context.Background(), `{"action":"status"}`)
	if err != nil {
		t.Fatalf("Execute(status): %v", err)
	}
	for _, want := range []string{
		"Rule firings:",
		"destructive_file_guard",
		"scope:sub-status",
		"1x (inject)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestSupervisorTool_StatusShowsDeniedCommand pins the deny's legibility
// in `supervisor status`: a blocked shell call reports the most recent
// command the guard refused and the reason it gave, beside the count — an
// operator reading the session afterwards sees what was blocked and why,
// not just that the guard fired. The detail is labelled last_cmd because it
// describes only the latest refusal; the count covers all of them.
func TestSupervisorTool_StatusShowsDeniedCommand(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })
	bus := kerneltrace.NewEffectBus(64)
	t.Cleanup(bus.Close)

	mgr := goatherd.NewScopeManager(store)
	if _, err := mgr.Create("sub-status", nil); err != nil {
		t.Fatalf("create scope: %v", err)
	}

	// The blocking shell guard on, with no allowlist: every shell call
	// is denied, which is the case an empty role allowlist produces.
	svc := supervision.NewService(mgr, bus, supervision.NewRegistry(),
		supervision.AdvisoryRules{}, true, false, "")

	const cmd = "rm -rf /tmp/probe"
	reason := svc.GuardCheck()("sub-status", "bash", `{"command":"`+cmd+`"}`)
	if reason == "" {
		t.Fatal("the shell guard must deny an unlisted command")
	}

	sup := &SupervisorTool{ScopeManager: mgr, Runs: supervision.NewRegistry(), Svc: svc}
	out, err := sup.Execute(context.Background(), `{"action":"status"}`)
	if err != nil {
		t.Fatalf("Execute(status): %v", err)
	}
	for _, want := range []string{
		"destructive_shell_guard",
		"scope:sub-status",
		"1x (deny)",
		`last_cmd="` + cmd + `"`,
		`reason="` + reason + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestSupervisorTool_StatusWithoutService pins the nil-service path:
// with no supervision service, status reports no firings honestly
// instead of guessing.
func TestSupervisorTool_StatusWithoutService(t *testing.T) {
	store, err := kerneltrace.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { store.Close(context.Background()) })

	mgr := goatherd.NewScopeManager(store)

	sup := &SupervisorTool{ScopeManager: mgr, Runs: supervision.NewRegistry()}
	out, err := sup.Execute(context.Background(), `{"action":"status"}`)
	if err != nil {
		t.Fatalf("Execute(status): %v", err)
	}
	if !strings.Contains(out, "Rule firings: 0 (supervision service not active)") {
		t.Errorf("status without a service must say so, got:\n%s", out)
	}
}
