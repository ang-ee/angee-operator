//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package cli

import (
	"time"

	"golang.org/x/sys/unix"
)

// termState captures the terminal mode saved before entering cbreak so the
// original mode can be restored on exit.
type termState struct {
	fd    int
	saved unix.Termios
}

// isTerminal reports whether fd refers to a terminal.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	return err == nil
}

// enterCbreak switches the terminal into cbreak mode: keypresses become
// readable one byte at a time (ICANON off) and are not echoed (ECHO off),
// while output post-processing (OPOST/ONLCR) and signal generation (ISIG, so
// Ctrl-C still raises SIGINT) are left on. That keeps the streamed child logs
// rendering with proper newlines and preserves the existing Ctrl-C teardown.
func enterCbreak(fd int) (*termState, error) {
	t, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	saved := *t
	t.Lflag &^= unix.ICANON | unix.ECHO
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, t); err != nil {
		return nil, err
	}
	return &termState{fd: fd, saved: saved}, nil
}

// restore returns the terminal to the mode captured by enterCbreak.
func (s *termState) restore() error {
	if s == nil {
		return nil
	}
	saved := s.saved
	return unix.IoctlSetTermios(s.fd, ioctlWriteTermios, &saved)
}

// pollReadable waits until fd has data to read or timeout elapses, returning
// whether input is ready. It lets the key reader wait for input without
// blocking indefinitely in Read, so it can observe context cancellation.
func pollReadable(fd int, timeout time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout/time.Millisecond))
	if err != nil {
		if err == unix.EINTR {
			// Interrupted by a signal (e.g. SIGWINCH); treat as "not ready" so
			// the caller loops and re-checks its context.
			return false, nil
		}
		return false, err
	}
	return n > 0, nil
}
