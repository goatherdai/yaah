package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// logBufSize caps the retained log output per process.
const logBufSize = 200 * 1024 // 200 KB

// logTruncatedMarker prefixes a log whose oldest output was dropped. The
// retained tail is the diagnostically useful part (a server/build's most
// recent output), so overflow drops the head, not the tail.
const logTruncatedMarker = "...[earlier output dropped]...\n"

// Info holds state for a single tracked background process.
type Info struct {
	ID          string
	Command     string
	Description string
	StartedAt   time.Time
	status      string // "running", "finished", "stopped", "error"
	logBuf      []byte // ring buffer of logBufSize bytes, allocated on first output
	logPos      int    // next write position in logBuf
	logLen      int    // valid bytes in logBuf (<= logBufSize)
	logDropped  bool   // at least one old byte was overwritten
	cmd         *exec.Cmd
	done        chan struct{} // closed once the process has exited and been reaped (or the start failed)
	mu          sync.Mutex
}

// Status returns the current lifecycle state under the same mutex used by
// the exit and stop paths. Callers outside this package must use this
// accessor; the field is unexported so an unsynchronized read cannot compile.
func (p *Info) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Done is closed once the process has exited and been reaped, or immediately
// if it failed to start. It is the race-free way to await exit: Status reports
// "stopped" as soon as a stop is requested, before the process is actually
// gone.
func (p *Info) Done() <-chan struct{} {
	return p.done
}

// Logs returns the retained process output. When the buffer overflowed, the
// oldest output is gone and the result starts with logTruncatedMarker; the
// newest output is always retained.
func (p *Info) Logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logLen == 0 {
		return ""
	}
	if !p.logDropped {
		// Without an overwrite the buffer is linear, even if a write ended
		// exactly at the cap (logPos == 0).
		return string(p.logBuf[:p.logLen])
	}
	return logTruncatedMarker + string(p.logBuf[p.logPos:]) + string(p.logBuf[:p.logPos])
}

// logsTruncated reports whether earlier output was dropped from the buffer.
// It distinguishes a process that produced nothing (false, empty Logs) from
// one whose head overflowed (true).
func (p *Info) logsTruncated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logDropped
}

// appendLog appends text to the ring buffer, retaining the newest logBufSize
// bytes. Overflow drops the oldest output rather than the tail.
func (p *Info) appendLog(s string) {
	if s == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLogLocked(s)
}

// appendLogLocked is appendLog for callers that already hold p.mu.
func (p *Info) appendLogLocked(s string) {
	if s == "" {
		return
	}
	if p.logBuf == nil {
		p.logBuf = make([]byte, logBufSize)
	}
	if len(s) >= logBufSize {
		dropped := p.logLen > 0 || len(s) > logBufSize
		copy(p.logBuf, s[len(s)-logBufSize:])
		p.logPos = 0
		p.logLen = logBufSize
		p.logDropped = p.logDropped || dropped
		return
	}
	for len(s) > 0 {
		n := copy(p.logBuf[p.logPos:], s)
		if p.logLen == logBufSize {
			p.logDropped = true
		}
		p.logPos = (p.logPos + n) % logBufSize
		p.logLen += n
		if p.logLen > logBufSize {
			p.logLen = logBufSize
		}
		s = s[n:]
	}
}

// ErrClosed is returned by Start after Close has run: the manager's owner
// quiesced the session, and no new process may outlive that decision.
var ErrClosed = errors.New("process manager is closed")

// Manager tracks all background processes.
type Manager struct {
	mu     sync.Mutex
	procs  map[string]*Info
	closed bool
	nextID atomic.Int64
}

// NewManager creates a new process manager.
func NewManager() *Manager {
	return &Manager{
		procs: make(map[string]*Info),
	}
}

// nextProcessID mints a unique process id. Add returns the new value, so
// concurrent callers cannot observe the same counter value.
func (m *Manager) nextProcessID() string {
	return fmt.Sprintf("proc-%d", m.nextID.Add(1))
}

// Start launches a command as a tracked background process. It returns an
// error wrapping ErrClosed once Close has run.
func (m *Manager) Start(command, description string) (*Info, error) {
	// Cheap pre-check so a start against a closed manager does no work. The
	// authoritative check runs under m.mu at registration below: the child
	// only becomes live at cmd.Start, and that is the instant whose race
	// with Close must resolve.
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("start %q: %w", command, ErrClosed)
	}

	// Use powershell on Windows, sh on Unix for the shell wrapper.
	// On Unix we must NOT prefer pwsh even if it is on PATH (GitHub Actions
	// ubuntu runners include pwsh, whose startup is ~1-2s and causes
	// trivial commands like `echo` to blow past test timeouts).
	// On Windows -NoProfile skips profile scripts: startup is faster and
	// deterministic regardless of the user's profile contents.
	shell, shellArgs := "sh", []string{"-c", command}
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("pwsh"); err == nil {
			shell = "pwsh"
			shellArgs = []string{"-NoProfile", "-Command", command}
		} else if _, err := exec.LookPath("powershell"); err == nil {
			shell = "powershell"
			shellArgs = []string{"-NoProfile", "-Command", command}
		}
	}

	cmd := exec.Command(shell, shellArgs...)
	cmd.Stdin = nil // close stdin so the child never blocks waiting for input
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	// Own process group (proc_unix.go): Stop and Close signal the whole
	// tree, so a shell wrapper's children die with it.
	setOwnProcessGroup(cmd)

	id := m.nextProcessID()

	info := &Info{
		ID:          id,
		Command:     command,
		Description: description,
		StartedAt:   time.Now(),
		status:      "running",
		cmd:         cmd,
		done:        make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		info.mu.Lock()
		info.status = "error"
		info.mu.Unlock()
		info.appendLog(fmt.Sprintf("start error: %v\n", err))
		// No exit goroutine is spawned on this path, so Done must be closed
		// here: an await on a failed start has to complete.
		close(info.done)
		return info, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// cmd.Start already ran, so the child is live but cannot be
		// registered: Close has swept the map and will never signal it. Kill
		// and reap it here — a refused start must not orphan the one process
		// it did spawn. No pipe readers were started on this path, so a
		// direct Wait is safe.
		if info.cmd.Process != nil {
			_ = signalTree(info.cmd.Process, syscall.SIGKILL)
		}
		_ = info.cmd.Wait()
		info.mu.Lock()
		info.status = "error"
		info.mu.Unlock()
		close(info.done)
		return nil, fmt.Errorf("start %q: %w", command, ErrClosed)
	}
	m.procs[id] = info
	m.mu.Unlock()

	// Read stdout/stderr in background, then wait for the process to exit.
	// Per Go docs, cmd.Wait must not be called before all pipe reads complete.
	var pipeWg sync.WaitGroup
	pipeWg.Add(2)
	go func() {
		defer pipeWg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			info.appendLog(scanner.Text() + "\n")
		}
	}()
	go func() {
		defer pipeWg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			info.appendLog(scanner.Text() + "\n")
		}
	}()

	go func() {
		pipeWg.Wait()
		err := cmd.Wait()
		info.mu.Lock()
		if info.status == "running" {
			if err != nil {
				info.status = "error"
				// Write directly to the ring buffer since we already hold
				// info.mu (appendLog would deadlock — sync.Mutex is not
				// reentrant).
				info.appendLogLocked(fmt.Sprintf("exit error: %v\n", err))
			} else {
				info.status = "finished"
			}
		}
		info.mu.Unlock()
		// Done closes after the status is final, so an observer of Done can
		// trust Status. This is the only closer on the started path.
		close(info.done)
	}()

	return info, nil
}

// List returns a copy of all tracked processes.
func (m *Manager) List() []*Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Info, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p)
	}
	return out
}

// Get returns a process by ID.
func (m *Manager) Get(id string) *Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[id]
}

// Stop terminates a running process.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	info := m.procs[id]
	m.mu.Unlock()
	if info == nil {
		return fmt.Errorf("process %s not found", id)
	}
	info.mu.Lock()
	if info.status != "running" {
		info.mu.Unlock()
		return fmt.Errorf("process %s is not running", id)
	}
	info.status = "stopped"
	info.mu.Unlock()

	if info.cmd != nil && info.cmd.Process != nil {
		return signalTree(info.cmd.Process, syscall.SIGKILL)
	}
	return nil
}

// CloseReport is what Close did.
type CloseReport struct {
	Live        int      // processes still running when Close began
	Exited      int      // of those, confirmed exited before Close returned
	Escalated   int      // of those, still alive after the grace period and hard-killed
	Unconfirmed []string // ids whose exit could not be confirmed (termination was attempted)
}

// hardKillSettle bounds how long Close waits for exits after the hard kill.
const hardKillSettle = 2 * time.Second

// Close stops every tracked process and refuses new ones. It is the teardown
// path: the session calls it once, after anything that could start a process
// has been quiesced. Live processes are asked to terminate and given until ctx
// expires to do so; the rest are hard-killed and given hardKillSettle more.
// The total wait is bounded by ctx plus hardKillSettle regardless of how many
// processes are live — all of them are signalled first and awaited together.
// Close is safe to call more than once; later calls are no-ops.
func (m *Manager) Close(ctx context.Context) CloseReport {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return CloseReport{}
	}
	m.closed = true
	var live []*Info
	for _, p := range m.procs {
		p.mu.Lock()
		wasRunning := p.status == "running"
		if wasRunning {
			// Claim the process before anyone else can: the exit goroutine
			// only rewrites a "running" status, so it stays "stopped" even
			// when it exits cleanly afterwards.
			p.status = "stopped"
		}
		p.mu.Unlock()
		if wasRunning && p.cmd != nil && p.cmd.Process != nil {
			live = append(live, p)
		}
	}
	m.mu.Unlock()

	report := CloseReport{Live: len(live)}
	if len(live) == 0 {
		return report
	}

	// Ask each live process to terminate. Individual delivery errors are
	// ignored: a process that exited between collection and the signal is
	// not an error, and Close's result is the report, not per-process
	// delivery receipts.
	for _, p := range live {
		_ = signalTree(p.cmd.Process, syscall.SIGTERM)
	}
	remaining := awaitExits(live, ctx.Done())
	if len(remaining) == 0 {
		report.Exited = len(live)
		return report
	}

	// Still alive when the caller's grace expired: hard-kill the survivors
	// and give them one bounded settle to be reaped.
	report.Escalated = len(remaining)
	for _, p := range remaining {
		_ = signalTree(p.cmd.Process, syscall.SIGKILL)
	}
	hardCtx, cancel := context.WithTimeout(context.Background(), hardKillSettle)
	defer cancel()
	open := awaitExits(remaining, hardCtx.Done())
	report.Exited = len(live) - len(open)
	if len(open) > 0 {
		report.Unconfirmed = make([]string, 0, len(open))
		for _, p := range open {
			report.Unconfirmed = append(report.Unconfirmed, p.ID)
		}
		sort.Strings(report.Unconfirmed)
	}
	return report
}

// awaitExits waits until every info has been reaped (done closed) or until
// trigger fires, and returns the infos still unconfirmed at that point. The
// wait is concurrent — one trigger bounds the whole set, not each member —
// and the joiner goroutine is cancelled when this function returns, so
// processes whose reap is never observed cannot leak it.
func awaitExits(infos []*Info, trigger <-chan struct{}) []*Info {
	if len(infos) == 0 {
		return nil
	}
	awaitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allExited := make(chan struct{})
	go func() {
		defer close(allExited)
		for _, p := range infos {
			select {
			case <-p.done:
			case <-awaitCtx.Done():
				return
			}
		}
	}()
	select {
	case <-allExited:
		return nil
	case <-trigger:
	}
	// The trigger and the last done can race: re-check non-blockingly so a
	// process that exited just inside the deadline is not reported open.
	var open []*Info
	for _, p := range infos {
		select {
		case <-p.done:
		default:
			open = append(open, p)
		}
	}
	return open
}
