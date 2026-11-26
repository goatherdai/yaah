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

// slowLogger writes render diagnostics to a file under the yaah home
// directory. stderr — the log package's default destination — is silenced
// while the TUI is on the alt screen (cmd/yaah/tui.go redirects it to
// /dev/null), so SLOW refreshMessages lines, the detector for exactly the
// stalls worth diagnosing, never reached anywhere persistent.
//
// Each line opens, appends, and closes the file: SLOW lines are rare (they
// only fire above the 50ms threshold), and a per-write open needs no close
// lifecycle on the app, keeps Windows tests free of open-handle locks on
// the temp directory, and lets a missing home directory heal itself. A
// failed open falls back to the log package, matching the old best-effort
// behavior; the yaah home is created if absent so first runs do not lose
// diagnostics into the stderr silencer.
type slowLogger struct {
	mu sync.Mutex
}

// slowLogName is the diagnostics file under config.HomeDir().
const slowLogName = "tui-render.log"

// slowLogMaxBytes caps the diagnostics file: when it is found larger on
// open, it is truncated. Diagnostics are per-run observations, not an
// archive; the latest run matters most.
const slowLogMaxBytes = 512 * 1024

func (l *slowLogger) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(time.Now().Format(time.RFC3339)+" "+format, args...)
	f, err := l.open()
	if err != nil {
		log.Print(msg)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, msg)
}

// open prepares the diagnostics file for one append: the yaah home is
// created if missing, and an oversized file is truncated so a stall-heavy
// session cannot grow it without bound.
func (l *slowLogger) open() (*os.File, error) {
	dir := config.HomeDir()
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
