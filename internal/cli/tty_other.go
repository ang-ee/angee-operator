//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package cli

import (
	"errors"
	"time"
)

// termState is a no-op on platforms without termios support; the interactive
// dev control loop is disabled there and `angee dev` just streams logs.
type termState struct{}

var errNoTTYControl = errors.New("interactive terminal control is not supported on this platform")

func isTerminal(int) bool { return false }

func enterCbreak(int) (*termState, error) { return nil, errNoTTYControl }

func (s *termState) restore() error { return nil }

func pollReadable(int, time.Duration) (bool, error) { return false, errNoTTYControl }
