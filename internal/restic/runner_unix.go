//go:build !windows

package restic

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureCommandCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
}

func terminateProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}
