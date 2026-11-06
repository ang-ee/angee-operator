//go:build !windows

package proccompose

import (
	"os/exec"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime/processgroup"
)

// Native foreground shutdown addresses its process group. Give transient work
// its own group so cancellation can never signal the hosting operator or shell.
func isolateForegroundCommand(command *exec.Cmd, waitDelay time.Duration) (func(), error) {
	return processgroup.Configure(command, waitDelay)
}

func transientJobSupport() error { return nil }
