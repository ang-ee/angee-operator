//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package cli

import "golang.org/x/sys/unix"

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
