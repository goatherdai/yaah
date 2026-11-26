package tui

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitForLog polls for the diagnostics file to hold want: the write is
// asynchronous (it must not block the tview event-loop goroutine), so the
// tests wait briefly instead of racing it.
func waitForLog(t *testing.T, path, want string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return data
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("diagnostics file not written: %v", err)
			}
			t.Fatalf("diagnostics file missing %q after 2s: %q", want, string(data))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncWriter is a goroutine-safe io.Writer for capturing log output from
// the slowLogger's asynchronous write goroutine.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestSlowLoggerWritesToYaahHome pins the diagnostics path: slow-render
// lines land in the yaah home directory, not on stderr, which the TUI
// silences on the alt screen. Each line opens, appends, and closes, so no
// open handle outlives the call and temp-dir cleanup is safe on Windows.
func TestSlowLoggerWritesToYaahHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)

	var l slowLogger
	l.printf("SLOW test %d", 42)
	l.printf("SLOW test %d", 43)

	path := filepath.Join(home, slowLogName)
	data := waitForLog(t, path, "SLOW test 43")
	if got := strings.Count(string(data), "SLOW test"); got != 2 {
		t.Fatalf("diagnostics file has %d SLOW lines, want 2: %q", got, string(data))
	}
	if !strings.Contains(string(data), "SLOW test 42") {
		t.Errorf("diagnostics file missing the first entry: %q", string(data))
	}
}

// TestSlowRenderWiring pins the wiring in refreshMessages: a slow refresh
// records a SLOW line in the diagnostics file. The threshold var makes the
// trigger deterministic instead of depending on wall-clock luck.
func TestSlowRenderWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	ui := New("test")
	// Negative: Go's monotonic clock on Windows has coarse granularity,
	// so a microsecond-fast refresh can measure totalDur == 0 and a 0
	// threshold would never fire. Below zero fires unconditionally. The
	// threshold is an App field, not package state, so the test can set
	// it directly.
	ui.slowThreshold = -time.Millisecond

	ui.appendMessage("wiring check")
	ui.refreshMessages()

	waitForLog(t, filepath.Join(home, slowLogName), "SLOW refreshMessages")
}

// TestSlowLoggerDropsWhenFull verifies the drop policy that keeps the
// event loop non-blocking: a full queue drops the line, counts it, and
// returns immediately.
func TestSlowLoggerDropsWhenFull(t *testing.T) {
	var l slowLogger
	l.ch = make(chan slowWrite, 1) // no writer running: the queue stays full
	l.enqueue(slowWrite{dir: "d", msg: "queued"})
	l.enqueue(slowWrite{dir: "d", msg: "dropped"})

	if got := l.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if len(l.ch) != 1 {
		t.Fatalf("queue length = %d, want 1 (the queued line)", len(l.ch))
	}
}

// TestSlowLoggerFlushDrains verifies the shutdown path: Flush lands every
// queued line before returning.
func TestSlowLoggerFlushDrains(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)

	var l slowLogger
	for i := 0; i < 10; i++ {
		l.printf("SLOW drain %d", i)
	}
	l.flush()

	data, err := os.ReadFile(filepath.Join(home, slowLogName))
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	if got := strings.Count(string(data), "SLOW drain"); got != 10 {
		t.Fatalf("flush landed %d of 10 lines", got)
	}
	if l.dropped.Load() != 0 {
		t.Errorf("dropped = %d, want 0", l.dropped.Load())
	}
}

// TestSlowLoggerCreatesMissingHome verifies the first-run path: a yaah home
// that does not exist yet is created, so diagnostics do not silently fall
// into the stderr silencer that motivates this file.
func TestSlowLoggerCreatesMissingHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "fresh", ".yaah")
	t.Setenv("YAAH_HOME", home)

	var l slowLogger
	l.printf("SLOW first run")

	waitForLog(t, filepath.Join(home, slowLogName), "SLOW first run")
}

// TestSlowLoggerFallsBackToLog verifies the fallback: when the diagnostics
// file cannot be opened, the line goes to the log package instead of being
// dropped. The blocker is a regular file where the home directory should
// be, so the open fails with ENOTDIR on every OS — a read-only directory
// does not deny file creation on Windows or when running as root.
func TestSlowLoggerFallsBackToLog(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("YAAH_HOME", blocker)

	captured := &syncWriter{}
	log.SetOutput(captured)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var l slowLogger
	l.printf("SLOW fallback %d", 7)

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(captured.String(), "SLOW fallback 7") {
		if time.Now().After(deadline) {
			t.Fatalf("fallback did not reach the log package; output: %q", captured.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSlowLoggerRotatesWhenLarge verifies the size cap: a diagnostics
// file past the cap is rotated aside rather than wiped, so the ramp into
// a stall stays recoverable while the fresh file starts clean.
func TestSlowLoggerRotatesWhenLarge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	path := filepath.Join(home, slowLogName)

	old := bytes.Repeat([]byte("OLD-RAMP\n"), slowLogMaxBytes/9+1)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatalf("seed oversized file: %v", err)
	}

	var l slowLogger
	l.printf("SLOW fresh %d", 1)

	data := waitForLog(t, path, "SLOW fresh 1")
	if int64(len(data)) > slowLogMaxBytes {
		t.Fatalf("fresh file oversized after write: %d bytes", len(data))
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if !bytes.Contains(rotated, []byte("OLD-RAMP")) {
		t.Errorf("rotated file lost the old ramp")
	}
}
