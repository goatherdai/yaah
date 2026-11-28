package process

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- NewManager ----------

func TestNewManager(t *testing.T) {
	m := NewManager()
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if len(m.List()) != 0 {
		t.Error("new manager should have empty process list")
	}
}

// ---------- Start, Get, List ----------

func TestStartSimpleCommand(t *testing.T) {
	m := NewManager()

	info, err := m.Start("echo hello", "test process")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if info == nil {
		t.Fatal("Start returned nil info")
	}
	defer m.Stop(info.ID)
	if info.ID == "" {
		t.Error("info.ID should not be empty")
	}
	status := info.Status()
	if status != "running" && status != "finished" {
		t.Errorf("info.Status() = %q; want %q or %q", status, "running", "finished")
	}
	if info.Command != "echo hello" {
		t.Errorf("info.Command = %q; want %q", info.Command, "echo hello")
	}
	if info.Description != "test process" {
		t.Errorf("info.Description = %q; want %q", info.Description, "test process")
	}

	// Verify it appears in List
	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List length = %d; want 1", len(list))
	}
	if list[0].ID != info.ID {
		t.Errorf("List[0].ID = %q; want %q", list[0].ID, info.ID)
	}

	// Verify Get works
	got := m.Get(info.ID)
	if got == nil {
		t.Fatal("Get returned nil for existing process")
	}
	if got.ID != info.ID {
		t.Errorf("Get.ID = %q; want %q", got.ID, info.ID)
	}

	// Get non-existent
	if m.Get("nonexistent") != nil {
		t.Error("Get should return nil for non-existent process")
	}
}

func TestStartEchoProducesOutput(t *testing.T) {
	m := NewManager()

	info, err := m.Start("echo line1; echo line2", "echo test")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if info == nil {
		t.Fatal("Start returned nil info")
	}
	defer m.Stop(info.ID)

	// Poll until the process finishes and logs accumulate. The generous
	// deadline tolerates slow shell startup on loaded CI runners
	// (pwsh cold start can exceed 2s on Windows).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := info.Status()
		if s != "running" {
			logs := info.Logs()
			if logs != "" {
				t.Logf("logs: %q", logs)
				return
			}
			t.Error("Logs should not be empty after running echo")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("echo process still running after 10s")
}

func TestMultipleProcesses(t *testing.T) {
	m := NewManager()

	for i := 0; i < 3; i++ {
		info, err := m.Start(fmt.Sprintf("echo proc%d", i),
			fmt.Sprintf("process %d", i))
		if err != nil {
			t.Fatalf("Start %d failed: %v", i, err)
		}
		defer m.Stop(info.ID)
	}

	list := m.List()
	if len(list) != 3 {
		t.Errorf("List length = %d; want 3", len(list))
	}

	// Verify all IDs are unique
	seen := make(map[string]bool)
	for _, p := range list {
		if seen[p.ID] {
			t.Errorf("duplicate ID: %s", p.ID)
		}
		seen[p.ID] = true
	}
}

func TestStartFailingCommand(t *testing.T) {
	m := NewManager()

	info, err := m.Start("exit 1", "failing test")
	if info == nil {
		t.Fatal("Start returned nil info")
	}
	defer m.Stop(info.ID)

	if err != nil {
		t.Logf("Start returned error: %v (continuing)", err)
	}

	// Poll for status change instead of a single sleep. The generous
	// deadline tolerates slow shell startup on loaded CI runners.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status := info.Status()
		if status != "running" {
			if status != "error" {
				t.Errorf("failing command status = %q; want %q", status, "error")
			}
			t.Logf("failing command status: %q, logs: %q", status, info.Logs())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	status := info.Status()
	t.Logf("failing command status: %q, logs: %q", status, info.Logs())
	t.Error("failing command status still 'running' after 10s")
}

// ---------- Stop ----------

func TestStopRunningProcess(t *testing.T) {
	m := NewManager()

	// Start a long-running sleep
	info, err := m.Start("Start-Sleep -Seconds 30", "sleep test")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Stop it
	err = m.Stop(info.ID)
	if err != nil {
		t.Errorf("Stop returned error: %v", err)
	}

	// Give it a moment to fully stop
	time.Sleep(200 * time.Millisecond)

	status := info.Status()
	if status != "stopped" {
		t.Errorf("status = %q; want %q", status, "stopped")
	}
}

func TestStopNonExistent(t *testing.T) {
	m := NewManager()
	err := m.Stop("nonexistent")
	if err == nil {
		t.Error("Stop should return error for non-existent process")
	}
}

func TestStopAlreadyStopped(t *testing.T) {
	m := NewManager()
	info, _ := m.Start("Start-Sleep -Seconds 5", "sleep test")

	_ = m.Stop(info.ID)
	time.Sleep(200 * time.Millisecond)

	// Second stop should fail
	err := m.Stop(info.ID)
	if err == nil {
		t.Error("second Stop should return error")
	}
}

// ---------- Logs ----------

func TestLogsInitiallyEmpty(t *testing.T) {
	m := NewManager()
	info, err := m.Start("Start-Sleep -Seconds 1", "logs test")
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	// Logs may be empty immediately (before output)
	logs := info.Logs()
	t.Logf("initial logs: %q", logs)
}

// TestManagerNextProcessIDConcurrent exercises the allocator used by Start.
// An Add-then-Load implementation can hand two callers the same id.
func TestManagerNextProcessIDConcurrent(t *testing.T) {
	m := NewManager()
	const n = 200
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- m.nextProcessID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, n)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate process id %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("got %d unique ids, want %d", len(seen), n)
	}
}

// ---------- Log buffer ----------

func TestInfoLogsEmptyVsTruncated(t *testing.T) {
	var p Info
	if got := p.Logs(); got != "" {
		t.Errorf("fresh Info.Logs() = %q, want empty", got)
	}
	if p.logsTruncated() {
		t.Error("fresh Info must not report truncation")
	}

	p.appendLog("")
	if p.logsTruncated() {
		t.Error("appending nothing must not report truncation")
	}
}

func TestInfoLogsExactlyAtCapacityIsNotTruncated(t *testing.T) {
	var p Info
	p.appendLog(strings.Repeat("a", logBufSize))

	if p.logsTruncated() {
		t.Error("a buffer filled exactly to capacity did not drop anything")
	}
	logs := p.Logs()
	if strings.HasPrefix(logs, logTruncatedMarker) {
		t.Error("exact-capacity log must not carry the truncation marker")
	}
	if len(logs) != logBufSize {
		t.Errorf("retained %d bytes, want %d", len(logs), logBufSize)
	}
}

// TestInfoLogsRetainsTail pins yaah-adt: overflow must discard the oldest
// output, keep the newest, and mark the loss — silently returning a stale
// prefix as if complete hid the diagnostic tail.
func TestInfoLogsRetainsTail(t *testing.T) {
	var p Info
	p.appendLog("HEAD\n")
	p.appendLog(strings.Repeat("x", logBufSize))
	p.appendLog("TAIL\n")

	if !p.logsTruncated() {
		t.Fatal("overflow must set logsTruncated")
	}
	logs := p.Logs()
	if !strings.HasPrefix(logs, logTruncatedMarker) {
		t.Errorf("truncated log missing marker: %q", logs[:60])
	}
	if !strings.HasSuffix(logs, "TAIL\n") {
		t.Error("newest output was dropped")
	}
	if strings.Contains(logs, "HEAD") {
		t.Error("oldest output was retained")
	}
	if max := logBufSize + len(logTruncatedMarker); len(logs) > max {
		t.Errorf("retained %d bytes, want at most %d", len(logs), max)
	}
}

func TestInfoLogsHandlesWrapAcrossAppends(t *testing.T) {
	var p Info
	p.appendLog(strings.Repeat("a", logBufSize-3))
	p.appendLog("bbbbbb") // 3 bytes fill the buffer, 3 overwrite the oldest.

	logs := p.Logs()
	if !strings.HasPrefix(logs, logTruncatedMarker) {
		t.Error("wrap must mark the log truncated")
	}
	if !strings.HasSuffix(logs, "bbb") {
		t.Errorf("wrap lost the newest bytes: tail %q", logs[len(logs)-10:])
	}
}

// ---------- Done ----------

// TestDoneClosesWhenStartFails pins the start-failure branch of the Done
// contract: a Start whose shell cannot even be found fails before any
// process exists, and the returned Info must still become awaitable — an
// await on a failed start would otherwise hang forever.
func TestDoneClosesWhenStartFails(t *testing.T) {
	m := NewManager()
	// An empty PATH makes exec.LookPath fail for the shell itself (neither
	// sh nor pwsh can be found), so cmd.Start fails before any process
	// exists.
	t.Setenv("PATH", t.TempDir())
	info, err := m.Start("echo hi", "unstartable")
	if err == nil {
		t.Fatal("Start succeeded with an empty PATH; expected a start failure")
	}
	if info == nil {
		t.Fatal("Start returned a nil Info alongside its error; the Done contract cannot be asserted")
	}
	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after a failed start")
	}
}

// ---------- Close ----------

// TestCloseStopsRunningProcess pins the headline behaviour: Close stops a
// live process, confirms its exit through Done, and reports it as Exited —
// not Escalated, because the polite signal is enough for a shell that does
// not ignore it.
func TestCloseStopsRunningProcess(t *testing.T) {
	m := NewManager()
	info, err := m.Start("sleep 30", "close test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep := m.Close(ctx)

	if rep.Live != 1 || rep.Exited != 1 || rep.Escalated != 0 || len(rep.Unconfirmed) != 0 {
		t.Errorf("Close report = %+v; want Live 1, Exited 1, Escalated 0, no Unconfirmed", rep)
	}
	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after Close")
	}
	if status := info.Status(); status != "stopped" {
		t.Errorf("status = %q; want %q", status, "stopped")
	}
}

// TestCloseRefusesNewStarts pins the closed guard: after Close the manager
// is terminal, and Start must fail with ErrClosed instead of spawning a
// process nobody will ever reap.
func TestCloseRefusesNewStarts(t *testing.T) {
	m := NewManager()
	m.Close(context.Background())

	if _, err := m.Start("echo hi", "after close"); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close: err = %v; want an error wrapping ErrClosed", err)
	}
}

// TestCloseOnIdleManagerIsANoOp pins the idempotence: an idle Close reports
// the zero report, and a second Close — the racing-teardown shape — is a
// silent no-op rather than a double teardown.
func TestCloseOnIdleManagerIsANoOp(t *testing.T) {
	m := NewManager()

	first := m.Close(context.Background())
	if first.Live != 0 || first.Exited != 0 || first.Escalated != 0 || first.Unconfirmed != nil {
		t.Errorf("first Close on an idle manager = %+v; want the zero report", first)
	}
	second := m.Close(context.Background())
	if second.Live != 0 || second.Exited != 0 || second.Escalated != 0 || second.Unconfirmed != nil {
		t.Errorf("second Close on an idle manager = %+v; want the zero report", second)
	}
}

// TestCloseIgnoresFinishedProcesses pins the collection filter: a process
// that already exited (Done closed, status final) is not live, so Close has
// nothing to signal and nothing to await.
func TestCloseIgnoresFinishedProcesses(t *testing.T) {
	m := NewManager()
	info, err := m.Start("echo done", "already finished")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Done closes only after the exit status is final, so observing it is
	// enough to know Close's collector will see a non-running status. The
	// generous deadline tolerates slow shell startup (pwsh cold start).
	select {
	case <-info.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("echo process did not finish within 20s")
	}

	rep := m.Close(context.Background())
	if rep.Live != 0 {
		t.Errorf("Close report = %+v; want Live 0 (a finished process is not live)", rep)
	}
}

// ---------- Close: process groups ----------

// waitForLogs polls until the process's output contains want, proving the
// shell progressed past the command that produced it. A signal sent before
// the child has armed its trap or forked its children hits default signal
// dispositions and would test the wrong thing — startup is racy, the marker
// is not.
func waitForLogs(t *testing.T, info *Info, want string) {
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

// TestStopKillsTheWholeTree witnesses the process group (yaah-i1c1): the
// shell forks sleep and then waits for it, so the tree is shell -> sleep.
// Killing only the shell leaves sleep holding the inherited stdout/stderr
// pipes — the pipe readers never see EOF, cmd.Wait never returns, and Done
// never closes. A group kill takes both down, which is exactly what this
// test observes. The "armed" marker (waitForLogs) closes the startup race:
// Stop only runs once sleep provably exists, so the witness cannot pass
// vacuously against a group that was still just the shell.
func TestStopKillsTheWholeTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only; Windows stops the direct child")
	}
	m := NewManager()
	info, err := m.Start("sleep 30 & echo armed; wait", "tree test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForLogs(t, info, "armed")

	if err := m.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-info.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close within 3s of Stop; the tree survived the kill")
	}
}

// TestCloseEscalatesToHardKill pins the escalation: a tree that ignores the
// polite signal must exhaust the caller's grace, be hard-killed, and still
// land in the report as Exited — Escalated counts the kill, not a failure to
// confirm.
func TestCloseEscalatesToHardKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only; Windows has no TERM for a tree to ignore")
	}
	m := NewManager()
	// The shell and its backgrounded sleep both ignore SIGTERM ("trap ''
	// TERM" and ignored dispositions survive fork+exec), so nothing exits
	// until Close escalates to SIGKILL.
	info, err := m.Start("trap '' TERM; sleep 30 & echo armed; wait", "stubborn tree")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The "armed" marker closes the startup race: Close must not signal
	// before the shell has armed its trap and forked the sleep — a TERM
	// delivered that early hits default dispositions and tests nothing.
	waitForLogs(t, info, "armed")

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rep := m.Close(ctx)
	elapsed := time.Since(start)

	if rep.Live != 1 || rep.Exited != 1 || rep.Escalated != 1 || len(rep.Unconfirmed) != 0 {
		t.Errorf("Close report = %+v; want Live 1, Exited 1, Escalated 1, no Unconfirmed", rep)
	}
	select {
	case <-info.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not close after the hard kill")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("Close returned in %s; the grace period was not waited out", elapsed)
	}
}
