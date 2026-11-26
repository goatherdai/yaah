package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlowLoggerWritesToYaahHome pins the diagnostics path: slow-render
// lines land in the yaah home directory, not on stderr, which the TUI
// silences on the alt screen.
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
	lines := strings.Count(string(data), "SLOW test")
	if lines != 2 {
		t.Fatalf("diagnostics file has %d SLOW lines, want 2: %q", lines, string(data))
	}
	if !strings.Contains(string(data), "SLOW test 42") || !strings.Contains(string(data), "SLOW test 43") {
		t.Errorf("diagnostics file missing entries: %q", string(data))
	}
}

// TestSlowLoggerFallsBackToLog verifies a failed open degrades to the log
// package instead of panicking or dropping the call.
func TestSlowLoggerFallsBackToLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("YAAH_HOME", filepath.Join(home, "blocked"))
	if err := os.MkdirAll(filepath.Join(home, "blocked"), 0o555); err != nil {
		t.Fatalf("mkdir read-only home: %v", err)
	}

	var l slowLogger
	l.printf("SLOW fallback %d", 7) // must not panic; goes to the log package
}
