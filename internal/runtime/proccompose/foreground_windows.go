//go:build windows

package proccompose

import (
	"os/exec"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

// Go cannot send os.Interrupt to Windows children. A process group alone does
// not provide tree cleanup, so refuse transient work until a native job-object
// lifecycle is implemented and verified on Windows.
func transientJobSupport() error {
	return &runtime.JobCapabilityError{Reason: "local capture jobs are unavailable on Windows until native process-tree cancellation is supported; use a container capture job"}
}
func isolateForegroundCommand(_ *exec.Cmd) error { return transientJobSupport() }
