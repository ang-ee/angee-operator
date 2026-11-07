//go:build windows

package compose

import "github.com/ang-ee/angee-operator/internal/runtime"

func streamJobSupport() error {
	return &runtime.JobCapabilityError{Reason: "streaming container jobs are unavailable on Windows because compose configuration requires inherited file descriptors and /dev/fd/3"}
}
