//go:build !windows

package proccompose

import (
	"os/exec"
	"syscall"
)

// Native foreground shutdown addresses its process group. Give transient work
// its own group so cancellation can never signal the hosting operator or shell.
func isolateForegroundCommand(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func transientJobSupport() error { return nil }
