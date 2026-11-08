//go:build windows

// Package processgroup configures an os/exec command for bounded, whole-group
// cancellation where the operating system supports it.
package processgroup

import (
	"os/exec"
	"time"
)

// Configure leaves CommandContext's native Windows cancellation in place.
// Windows process-tree ownership requires a job object; callers that require
// whole-tree cleanup must refuse the operation before starting the command.
func Configure(_ *exec.Cmd, _ time.Duration) (func(), error) {
	return func() {}, nil
}

// ConfigureImmediateKill leaves CommandContext's native Windows cancellation
// in place. Callers still receive a completion function for portable cleanup.
func ConfigureImmediateKill(_ *exec.Cmd) (func(), error) {
	return func() {}, nil
}
