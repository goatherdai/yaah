//go:build unix

package process

import (
	"os"
	"os/exec"
	"syscall"
)

// setOwnProcessGroup puts the child in a process group of its own so Stop and
// Close can signal the whole tree: a shell wrapper that spawned children
// (sh -c "server & wait") must not leave them running when the shell dies.
// Must be called before cmd.Start.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalTree delivers sig to the process group led by p (the negated pid
// addresses the group). Only an unreaped leader is signalled this way, so its
// pid — and with it the group id — cannot have been reused. If the group is
// gone, fall back to the direct process.
func signalTree(p *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-p.Pid, sig); err == nil {
		return nil
	}
	return p.Signal(sig)
}
