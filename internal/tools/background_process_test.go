package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/goatherdai/yaah/internal/process"
)

// TestBackgroundProcessStartFailureIsError pins yaah-gvv: a launch failure is
// a failed tool call, not a successful one carrying prose.
func TestBackgroundProcessStartFailureIsError(t *testing.T) {
	// An empty PATH makes the shell wrapper itself unresolvable, so Start
	// fails before any command can run.
	t.Setenv("PATH", "")

	tool := &BackgroundProcessTool{Manager: process.NewManager()}
	result, err := tool.Execute(context.Background(), `{"action":"start","command":"echo hi"}`)
	if err == nil {
		t.Fatalf("start failure returned a nil error (result %q)", result)
	}
	if !strings.Contains(err.Error(), "start") || !strings.Contains(err.Error(), "echo hi") {
		t.Errorf("error should name the action and command: %v", err)
	}
}

func TestBackgroundProcess_IsDangerous(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"start", `{"action":"start","command":"echo hi"}`, true},
		{"restart", `{"action":"restart","id":"p1"}`, true},
		{"empty action", `{}`, true},
		{"unknown action", `{"action":"explode"}`, true},
		{"invalid json", `{not json`, true},
		{"list", `{"action":"list"}`, false},
		{"status", `{"action":"status","id":"p1"}`, false},
		{"logs", `{"action":"logs","id":"p1"}`, false},
		{"stop", `{"action":"stop","id":"p1"}`, false},
	}
	tool := &BackgroundProcessTool{}
	for _, tc := range cases {
		if got := tool.IsDangerous(tc.args); got != tc.want {
			t.Errorf("IsDangerous(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackgroundProcess_RefusesDangerousStart(t *testing.T) {
	mgr := process.NewManager()
	tool := &BackgroundProcessTool{Manager: mgr}
	// mkfs in verb position is a hard refusal whatever follows it.
	_, err := tool.Execute(context.Background(), `{"action":"start","command":"mkfs.ext4 /dev/yaah-test-nonexistent"}`)
	if err == nil || !strings.Contains(err.Error(), "dangerous pattern") {
		t.Fatalf("start of a dangerous-pattern command = %v, want a refusal", err)
	}
	if procs := mgr.List(); len(procs) != 0 {
		t.Errorf("refused start launched %d process(es)", len(procs))
	}
}

// TestBackgroundProcess_AllowsReadOnlyDataInStart pins yaah-0lx8 on this tool:
// the deny-list gates the verb, so a read-only command whose arguments carry
// deny-list vocabulary starts normally.
func TestBackgroundProcess_AllowsReadOnlyDataInStart(t *testing.T) {
	mgr := process.NewManager()
	tool := &BackgroundProcessTool{Manager: mgr}
	result, err := tool.Execute(context.Background(), `{"action":"start","command":"echo 'rm -rf /; mkfs; shutdown -h now'"}`)
	if err != nil {
		t.Fatalf("start of a read-only command with scary arguments = %v, want it to run", err)
	}
	if !strings.Contains(result, "Started process") {
		t.Errorf("result = %q, want a started process", result)
	}
	for _, p := range mgr.List() {
		t.Cleanup(func() { _ = mgr.Stop(p.ID) })
	}
}

func TestBackgroundProcess_RefusesDangerousRestartWithoutStopping(t *testing.T) {
	mgr := process.NewManager()
	// The second segment is a hard refusal and the first keeps the process
	// alive, so a restart that failed to re-check would be observable as a
	// stopped process. Planting goes through the manager, not the tool.
	info, err := mgr.Start("sleep 30; mkfs", "planted")
	if err != nil {
		t.Fatalf("plant process: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop(info.ID) })

	tool := &BackgroundProcessTool{Manager: mgr}
	_, err = tool.Execute(context.Background(), `{"action":"restart","id":"`+info.ID+`"}`)
	if err == nil || !strings.Contains(err.Error(), "dangerous pattern") {
		t.Fatalf("restart of a dangerous-pattern command = %v, want a refusal", err)
	}
	if got := mgr.Get(info.ID); got == nil || got.Status() != "running" {
		t.Fatalf("refused restart changed the process state: %+v", got)
	}
}
