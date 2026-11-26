package tui

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	data, err := os.ReadFile(filepath.Join(home, slowLogName))
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	if got := strings.Count(string(data), "SLOW test"); got != 2 {
		t.Fatalf("diagnostics file has %d SLOW lines, want 2: %q", got, string(data))
	}
	if !strings.Contains(string(data), "SLOW test 42") || !strings.Contains(string(data), "SLOW test 43") {
		t.Errorf("diagnostics file missing entries: %q", string(data))
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

	if _, err := os.Stat(filepath.Join(home, slowLogName)); err != nil {
		t.Fatalf("diagnostics file not created in a missing home: %v", err)
	}
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

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var l slowLogger
	l.printf("SLOW fallback %d", 7)

	if !strings.Contains(buf.String(), "SLOW fallback 7") {
		t.Errorf("fallback did not reach the log package; output: %q", buf.String())
	}
}

// TestSlowLoggerTruncatesWhenLarge verifies the size cap: a diagnostics
// file past the cap is truncated on the next write rather than growing
// without bound across a stall-heavy session.
func TestSlowLoggerTruncatesWhenLarge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", home)
	path := filepath.Join(home, slowLogName)

	old := bytes.Repeat([]byte("x"), slowLogMaxBytes+1)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatalf("seed oversized file: %v", err)
	}

	var l slowLogger
	l.printf("SLOW fresh %d", 1)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	if int64(len(data)) > slowLogMaxBytes {
		t.Fatalf("file still oversized after write: %d bytes", len(data))
	}
	if !strings.Contains(string(data), "SLOW fresh 1") {
		t.Errorf("fresh entry missing after truncate: %d bytes", len(data))
	}
}
