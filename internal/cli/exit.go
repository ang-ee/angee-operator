package cli

import (
	"errors"

	"github.com/ang-ee/angee-operator/internal/cli/inputform"
)

// ExitCode maps a command error to its process exit status.
func ExitCode(err error) int {
	if errors.Is(err, inputform.ErrAborted) {
		return 130
	}
	return 1
}
