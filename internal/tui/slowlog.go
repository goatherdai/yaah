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
// stalls worth diagnosing, never reached anywhere persistent. The file is
// opened lazily on first write and appended to across runs; a failed open
// falls back to the log package, matching the old best-effort behavior.
type slowLogger struct {
	mu   sync.Mutex
	file *os.File
}

// slowLogName is the diagnostics file under config.HomeDir().
const slowLogName = "tui-render.log"

func (l *slowLogger) printf(format string, args ...any) {
	f := l.writer()
	msg := fmt.Sprintf(time.Now().Format(time.RFC3339)+" "+format, args...)
	if f == nil {
		log.Print(msg)
		return
	}
	fmt.Fprintln(f, msg)
}

// writer returns the open diagnostics file, or nil if it could not be
// opened (the caller then falls back to the log package).
func (l *slowLogger) writer() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file
	}
	f, err := os.OpenFile(filepath.Join(config.HomeDir(), slowLogName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	l.file = f
	return f
}
