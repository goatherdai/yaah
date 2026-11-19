package janitor

import "os/exec"

func execCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}
