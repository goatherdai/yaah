package tui

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goatherdai/yaah/internal/config"
)

// slowRenderThreshold is the refresh duration above which a SLOW line is
// recorded. A var so tests can pin the wiring without wall-clock luck.
var slowRenderThreshold = 50 * time.Millisecond

// slowLogger writes render diagnostics to a file under the yaah home
// directory. stderr — the log package's default destination — is silenced
// while the TUI is on the alt screen (cmd/yaah/tui.go redirects it to
// /dev/null), so SLOW refreshMessages lines, the detector for exactly the
// stalls worth diagnosing, never reached anywhere persistent.
//
// Lines flow through a small buffered channel to a single writer
// goroutine: the tview event-loop goroutine never touches the
// filesystem, a slow or stalled home directory cannot pile up goroutines
// or open descriptors, and memory is capped by the channel. When the
// queue is full — a refresh storm worse than the diagnostics can record —
// lines are dropped: best-effort by design. The destination directory is
// resolved at printf time so a write can never drift to a different home
// after the environment changes. Any failure falls back to the log
// package; the yaah home is created if absent so first runs do not lose
// diagnostics into the stderr silencer. The writer goroutine is
// deliberately never shut down: a few syscalls per line is cheaper than a
// shutdown protocol on the app, and the channel bounds what can be in
// flight when the process exits.
type slowLogger struct {
	once sync.Once
	ch   chan slowWrite
}

// slowWrite is one queued diagnostics line with the home directory
// resolved when the line was emitted.
type slowWrite struct {
	dir string
	msg string
}

// slowLogQueue bounds in-flight diagnostics lines.
const slowLogQueue = 64

// slowLogName is the diagnostics file under the resolved home directory.
const slowLogName = "tui-render.log"

// slowLogMaxBytes caps the diagnostics file: when it is found larger on
// open, it is truncated. Diagnostics are per-run observations, not an
// archive; the latest run matters most.
const slowLogMaxBytes = 512 * 1024

func (l *slowLogger) printf(format string, args ...any) {
	// Format the caller's message first so vet and staticcheck can verify
	// the format arguments; only the prefix is added afterwards.
	msg := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...)
	dir := config.HomeDir()
	l.once.Do(func() {
		l.ch = make(chan slowWrite, slowLogQueue)
		go l.drain()
	})
	select {
	case l.ch <- slowWrite{dir: dir, msg: msg}:
	default: // queue full: drop rather than pile up
	}
}

// drain serializes diagnostics writes for the life of the process.
func (l *slowLogger) drain() {
	for w := range l.ch {
		l.write(w)
	}
}

func (l *slowLogger) write(w slowWrite) {
	f, err := l.open(w.dir)
	if err != nil {
		log.Print(w.msg)
		return
	}
	_, werr := fmt.Fprintln(f, w.msg)
	// Close even when the write failed: a full disk or quota is exactly
	// when the SLOW path fires repeatedly, and skipping close there would
	// leak descriptors until every later open fails.
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// The line never landed; the log fallback is at least visible
		// where stderr is not silenced.
		log.Print(w.msg)
	}
}

// open prepares the diagnostics file for one append: the home directory
// is created if missing, and an oversized file is truncated so a
// stall-heavy session cannot grow it without bound.
func (l *slowLogger) open(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, slowLogName)
	if st, err := os.Stat(path); err == nil && st.Size() > slowLogMaxBytes {
		if err := os.Truncate(path, 0); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
