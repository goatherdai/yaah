package yaah

// session_process_teardown_test.go pins the yaah-i1c1 teardown step: close()
// stops the session's background processes (process.Manager.Close) AFTER the
// background sub-agent drain has quiesced the things that could start one,
// and within the session-owned procCloseTimeout bound. The tests run the
// REAL teardown on a minimal &agentSession{}, like session_close_test.go.

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goatherdai/yaah/internal/jobs"
	processpkg "github.com/goatherdai/yaah/internal/process"
)

// waitForProcessLogs polls until the process's output contains want,
// proving the shell progressed past the command that produced it. A signal
// sent before the child has armed its trap or forked its children hits
// default signal dispositions and would test the wrong thing — startup is
// racy, the marker is not.
func waitForProcessLogs(t *testing.T, info *processpkg.Info, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(info.Logs(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process output never contained %q; last logs: %q", want, info.Logs())
}

// TestSessionCloseStopsBackgroundProcesses pins the headline behaviour: a
// process started before close() is stopped by it, and confirmed gone
// through Info.Done — the status flag alone is set before the kill lands,
// so only Done is evidence the process is actually reaped.
func TestSessionCloseStopsBackgroundProcesses(t *testing.T) {
	m := processpkg.NewManager()
	info, err := m.Start("sleep 30", "orphan")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	sess := &agentSession{procMgr: m}
	sess.close()

	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the background process was still alive after close()")
	}
	if status := info.Status(); status != "stopped" {
		t.Errorf("status = %q; want %q", status, "stopped")
	}
}

// TestSessionCloseKeepsProcessesAliveThroughTheDrain pins the ORDER: the
// drain quiesces the sub-agents (the only other process spawner) first, and
// the process manager closes after — a runner cancelled by the drain can
// still observe its session's background process alive, and close() stops
// that process only once the drain has joined.
func TestSessionCloseKeepsProcessesAliveThroughTheDrain(t *testing.T) {
	m := processpkg.NewManager()
	info, err := m.Start("sleep 30", "drain probe")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	bg := jobs.NewBackgroundJobs()
	// Written before the runner returns; close() joins the runner's
	// goroutine inside CloseAndWait before returning, so the plain write is
	// visible after close() without further synchronisation.
	liveDuringDrain := false
	runner := func(ctx context.Context, prompt string, p jobs.SubAgentParams) (string, error) {
		<-ctx.Done()
		select {
		case <-info.Done():
		default:
			liveDuringDrain = true
		}
		return "", ctx.Err()
	}
	if _, err := bg.Launch(context.Background(), runner, "worker", "drain-probe", "x", jobs.SubAgentParams{Role: "worker"}, 0); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	sess := &agentSession{backgroundJobs: bg, procMgr: m}
	sess.close()

	if !liveDuringDrain {
		t.Error("the process was already gone while the drain ran; the manager closed too early")
	}
	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the background process survived close()")
	}
}

// TestSessionCloseIsBoundedWhenAProcessIgnoresTerm pins the bound: a tree
// that ignores the polite signal must not stretch close() — the session's
// grace expires, Close escalates to the hard kill, and teardown returns,
// having actually reaped the process.
func TestSessionCloseIsBoundedWhenAProcessIgnoresTerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only; Windows stops the direct child and has no TERM for a tree to ignore")
	}
	m := processpkg.NewManager()
	info, err := m.Start("trap '' TERM; sleep 30 & echo armed; wait", "ignores the polite signal")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The "armed" marker closes the startup race: close() must not signal
	// before the shell has armed its trap and forked the sleep — a TERM
	// delivered that early hits default dispositions and the process would
	// die politely, proving nothing about the bound.
	waitForProcessLogs(t, info, "armed")

	sess := &agentSession{procMgr: m, procCloseTimeout: 100 * time.Millisecond}
	start := time.Now()
	sess.close()
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("close() took %s; a process that ignores termination must not stretch teardown", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("close() returned in %s; the %s grace was not waited out", elapsed, sess.procCloseTimeout)
	}
	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the background process survived close()")
	}
}
