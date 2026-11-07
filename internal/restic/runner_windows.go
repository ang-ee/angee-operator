//go:build windows

package restic

import (
	"os"
	"os/exec"
	"time"
)

func configureCommandCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = time.Second
}

func terminateProcess(process *os.Process) error {
	return process.Kill()
}
