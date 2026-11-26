package tui

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goatherdai/yaah/internal/atomicfile"
	"github.com/goatherdai/yaah/internal/config"
)

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
// lines are dropped, counted in slowLogger.dropped and surfaced on the
// tui.refresh span; a log that looks complete must be read together with
// that counter. The destination directory is resolved at printf time so
// a write can never drift to a different home after the environment
// changes.
//
// Failures fall back to the log package — which the TUI also silences on
// the alt screen, so they are additionally counted in slowLogger.failed, the
// number of lines that landed nowhere; both counters are exported on the
// tui.refresh span. The yaah home is created if absent so first runs do
// not lose diagnostics into the silencer. Flush drains the queue before
// the alt screen is torn down; the writer goroutine ends there.
type slowLogger struct {
	once    sync.Once
	ch      chan slowWrite
	wg      sync.WaitGroup
	dropped atomic.Int64
	failed  atomic.Int64
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
// open, it is rotated aside and a fresh file starts. Diagnostics are
// per-run observations, not an archive; the rotation keeps the ramp into
// a stall (the lines a bug report needs) one rename away instead of
// wiped.
const slowLogMaxBytes = 512 * 1024

func (l *slowLogger) printf(format string, args ...any) {
	// Format the caller's message first so vet and staticcheck can verify
	// the format arguments; only the prefix is added afterwards.
	msg := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...)
	dir := config.HomeDir()
	l.once.Do(func() {
		l.ch = make(chan slowWrite, slowLogQueue)
		l.wg.Add(1)
		go l.drain()
	})
	l.enqueue(slowWrite{dir: dir, msg: msg})
}

// enqueue hands one line to the writer, dropping it — counted — when the
// queue is full. Never blocks.
func (l *slowLogger) enqueue(w slowWrite) {
	select {
	case l.ch <- w:
	default:
		l.dropped.Add(1)
	}
}

// drain serializes diagnostics writes until Flush closes the channel.
func (l *slowLogger) drain() {
	defer l.wg.Done()
	for w := range l.ch {
		l.write(w)
	}
}

// Flush closes the queue and waits for the writer to finish, so lines
// still in flight land before the alt screen is torn down and stderr is
// restored. Safe to call once at app shutdown; later printf calls restart
// the writer.
func (l *slowLogger) flush() {
	if l.ch == nil {
		return // never started, or already flushed
	}
	close(l.ch)
	l.wg.Wait()
	l.once = sync.Once{}
	l.ch = nil
}

func (l *slowLogger) write(w slowWrite) {
	f, err := l.open(w.dir)
	if err != nil {
		l.failed.Add(1)
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
		l.failed.Add(1)
		log.Print(w.msg)
	}
}

// open prepares the diagnostics file for one append: the home directory
// is created if missing, and an oversized file is rotated aside so a
// stall-heavy session cannot grow it without bound. The rotation is
// best-effort — Windows can transiently refuse a rename while a scanner
// holds the fresh file (the contention internal/atomicfile retries) — and
// a failed rotation must not cost the diagnostic line: appending to the
// oversized file is strictly better than losing the line.
func (l *slowLogger) open(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, slowLogName)
	if st, err := os.Stat(path); err == nil && st.Size() > slowLogMaxBytes {
		_ = atomicfile.Replace(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
