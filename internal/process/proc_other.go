//go:build !unix

package process

import (
	"os"
	"os/exec"
	"syscall"
)

// setOwnProcessGroup is a no-op: Windows has no process groups and
// os.Interrupt is unsupported there, so Stop and Close can only terminate the
// direct child. Killing the whole tree on Windows needs a job object (not
// implemented).
func setOwnProcessGroup(cmd *exec.Cmd) {}

// signalTree terminates the direct process only (see setOwnProcessGroup):
// without a process group there is no tree to address, so the polite signal
// is ignored and the hard kill is the only portable termination primitive.
func signalTree(p *os.Process, _ syscall.Signal) error {
	return p.Kill()
}
