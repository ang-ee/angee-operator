package runtime

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
)

// maxLogLine bounds a single scanned log line so a pathologically long line
// can't blow bufio.Scanner's default 64 KiB token limit and abort the stream.
const maxLogLine = 1024 * 1024

// StreamCommand starts cmd with its stdout and stderr merged into one pipe and
// returns a channel that yields one newline-terminated line per scanned line as
// the process produces it, closing the channel when the process exits or ctx is
// done. This is the live-follow counterpart to a buffered CombinedOutput: lines
// surface incrementally instead of all at once on exit.
//
// The caller configures cmd (path, dir, env) but must NOT set Stdout/Stderr.
// Cancelling ctx kills the process (via exec.CommandContext). The command's
// exit status is intentionally ignored — a killed follow process exiting
// non-zero is the normal way the stream ends.
//
// Teardown does not depend on the killed child closing the write pipe: a
// watcher closes the read end on ctx cancel, so a Scan() blocked in Read
// unblocks at once even if a grandchild inherited the fd and outlived the
// parent. That makes teardown prompt and leak-free regardless of the child's
// process tree.
func StreamCommand(ctx context.Context, cmd *exec.Cmd) (<-chan string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	// Close the parent's copy of the write end so the reader sees EOF once the
	// child (the only remaining writer) exits.
	_ = pw.Close()

	ch := make(chan string)
	go func() {
		defer close(ch)
		defer func() { _ = pr.Close() }()
		// Watcher: on ctx cancel, close the read end to unblock a Scan() stuck
		// in Read; on a natural stream end, `stop` releases the watcher. Closing
		// pr twice (here and the deferred close) is harmless on *os.File.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				_ = pr.Close()
			case <-stop:
			}
		}()
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLogLine)
		for scanner.Scan() {
			select {
			case ch <- scanner.Text() + "\n":
			case <-ctx.Done():
				_ = cmd.Wait()
				return
			}
		}
		_ = cmd.Wait()
	}()
	return ch, nil
}

// ReplayLines turns already-captured output into a line channel, mirroring
// StreamCommand's framing. Backends use it when a non-exec (test) Runner stands
// in for a live process and can only return captured bytes.
func ReplayLines(ctx context.Context, out []byte) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		scanner.Buffer(make([]byte, 0, 64*1024), maxLogLine)
		for scanner.Scan() {
			select {
			case ch <- scanner.Text() + "\n":
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
