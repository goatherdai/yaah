package process

// signal_test.go pins the reuse-safety of Info.signal and the start race
// that Close can lose (yaah-i1c1, review round 1). The group form of a signal
// names a process by pid, and a pid can be reused the moment cmd.Wait has
// reaped it — so signal must only use the group form while the process is
// provably unreaped.

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// liveSleep starts a bare `sleep 30` outside the manager, so the test owns
// its Info and can put that Info into a state the manager never produces on
// its own. Liveness is observed through cmd.Wait — it returns only once the
// process has exited; signal(0) cannot tell a live process from a killed but
// unreaped one. exited reports that, exitedSoon polls it, and reap cleans
// up. Only reap reads the Wait result, so the tests cannot consume it from
// under each other.
func liveSleep(t *testing.T) (info *Info, exited func() bool, exitedSoon func(time.Duration) bool, reap func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("os.Process.Signal is not supported on Windows")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start sleep: %v", err)
	}
	var gone atomic.Bool
	exitedSoon = func(d time.Duration) bool {
		deadline := time.Now().Add(d)
		for !gone.Load() {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(10 * time.Millisecond)
		}
		return true
	}
	go func() {
		_ = cmd.Wait()
		gone.Store(true)
	}()
	return &Info{ID: "bare", cmd: cmd, done: make(chan struct{})},
		gone.Load,
		exitedSoon,
		func() {
			_ = cmd.Process.Kill()
			exitedSoon(3 * time.Second) // best effort; the test's own verdict does not depend on it
		}
}

// stillRunning asserts the process has not exited for the whole window.
func stillRunning(t *testing.T, exitedSoon func(time.Duration) bool, what string) {
	t.Helper()
	if exitedSoon(300 * time.Millisecond) {
		t.Fatal(what)
	}
}

// A process that is done has been reaped; its pid is free. signal must not
// touch it, whatever the state of waitMu.
func TestSignalNeverAddressesAReapedProcess(t *testing.T) {
	info, _, exitedSoon, reap := liveSleep(t)
	defer reap()
	close(info.done) // the exit goroutine's last act

	if err := info.signal(syscall.SIGKILL); err != nil {
		t.Fatalf("signal on a reaped process: %v", err)
	}
	stillRunning(t, exitedSoon, "signal killed through a pid it must not have addressed")
}

// While the exit goroutine is inside cmd.Wait (waitMu held), the process is
// live with its pipes closed or mid-reap. The direct os.Process signal is
// the only reuse-safe form there, and it has to actually reach the process:
// a server that redirected its output must still be stoppable.
func TestSignalUsesTheDirectProcessWhileWaitIsInFlight(t *testing.T) {
	info, _, exitedSoon, reap := liveSleep(t)
	defer reap()
	info.waitMu.Lock() // the exit goroutine's state during cmd.Wait
	defer info.waitMu.Unlock()

	if err := info.signal(syscall.SIGKILL); err != nil {
		t.Fatalf("signal during Wait: %v", err)
	}
	if !exitedSoon(3 * time.Second) {
		t.Fatal("the direct signal did not reach the process: Wait still pending")
	}
}

// End to end through Stop: a command that closes its inherited pipes and
// keeps running (the exit goroutine enters cmd.Wait long before the process
// exits) is still stopped. Skip on Windows: sh redirection syntax.
func TestStopReachesAProcessThatClosedItsPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh redirection syntax; Windows stops the direct child")
	}
	m := NewManager()
	info, err := m.Start("echo closed; exec sleep 30 >/dev/null 2>&1", "closed its pipes")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForLogs(t, info, "closed")
	// Let the pipes reach EOF so the exit goroutine is inside cmd.Wait — the
	// state where only the direct signal is allowed. Either state must end
	// the process; this just makes the test exercise the interesting one.
	time.Sleep(200 * time.Millisecond)

	if err := m.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-info.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a process that closed its pipes survived Stop")
	}
}

// The invariant signal rests on: once the exit goroutine is inside cmd.Wait,
// waitMu is held until Done. Observed on a live process with closed pipes —
// the one state where the goroutine waits for a long time. Before EOF the
// lock is legitimately free, so the test polls for the hand-over instead of
// asserting a timing.
func TestWaitMuIsHeldWhileTheProcessIsAwaited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh redirection syntax; Windows stops the direct child")
	}
	m := NewManager()
	info, err := m.Start("echo closed; exec sleep 30 >/dev/null 2>&1", "awaited")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close(context.Background())
	waitForLogs(t, info, "closed")

	deadline := time.Now().Add(3 * time.Second)
	for {
		if !info.waitMu.TryLock() {
			break // held by the exit goroutine: the hand-over happened
		}
		info.waitMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("waitMu was never taken while the process was awaited")
		}
		select {
		case <-info.Done():
			t.Fatal("the process exited on its own; the test wanted it alive")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !alive(t, info) {
		t.Fatal("the process is gone, so a held waitMu would mean nothing")
	}
}

// alive reports whether a manager-started process is still writing to its
// pipes: a process that closed them cannot be probed this way, which is why
// the bare-process tests use cmd.Wait instead.
func alive(t *testing.T, info *Info) bool {
	t.Helper()
	select {
	case <-info.Done():
		return false
	case <-time.After(50 * time.Millisecond):
		return true
	}
}

// A Start that is past cmd.Start when Close runs cannot register its child.
// It must refuse, and the child it did spawn must be gone — Done closed is
// the proof, since the refusal path reaps it before returning.
func TestStartThatLosesTheRaceWithClose(t *testing.T) {
	m := NewManager()
	m.afterStart = func() { m.Close(context.Background()) }

	info, err := m.Start("sleep 30", "loses the race")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after a racing Close: err = %v; want ErrClosed", err)
	}
	if info == nil {
		t.Fatal("a refused start must return the Info of the child it spawned, so the caller can see it is gone")
	}
	select {
	case <-info.Done():
	default:
		t.Fatal("the child spawned by the refused start was not reaped")
	}
	if status := info.Status(); status != "error" {
		t.Errorf("status = %q; want %q", status, "error")
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("%d process(es) registered after a refused start; want 0", n)
	}
}
