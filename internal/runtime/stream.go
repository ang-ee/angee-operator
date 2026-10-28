package runtime

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/ang-ee/angee-operator/internal/logctx"
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
func StreamCommand(ctx context.Context, cmd *exec.Cmd, attrs ...slog.Attr) (<-chan string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	name := cmd.Path
	args := cmd.Args
	if len(cmd.Args) > 0 {
		name = cmd.Args[0]
		args = cmd.Args[1:]
	}
	trace := logctx.TraceExec(ctx, name, args, cmd.Dir, attrs...)
	if err := cmd.Start(); err != nil {
		trace(nil, err)
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
				trace(nil, cmd.Wait())
				return
			}
		}
		trace(nil, cmd.Wait())
	}()
	return ch, nil
}

// MergeStreams interleaves channels into one as their elements arrive, so a live
// stream from one doesn't wait for another to drain. The result closes once
// every input has closed, or when ctx is done.
func MergeStreams(ctx context.Context, channels ...<-chan string) <-chan string {
	out := make(chan string)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch <-chan string) {
			defer wg.Done()
			for line := range ch {
				select {
				case out <- line:
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// PrefixLines prefixes each line of text with label, padded to width, and a
// `|` separator, as docker compose labels the lines of several services. The
// text keeps its line endings, and an unterminated last line is prefixed too.
func PrefixLines(text, label string, width int) string {
	if text == "" {
		return ""
	}
	prefix := label + strings.Repeat(" ", max(width-len(label), 0)) + " | "
	var b strings.Builder
	for line := range strings.SplitAfterSeq(text, "\n") {
		if line == "" {
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
	}
	return b.String()
}

// StripTerminalControls removes the ECMA-48 escape sequences a terminal acts
// on from captured output, so a receipt carries only its text: control
// sequences (CSI, ESC [ … final byte), such as colours; operating system
// commands (OSC, ESC ] … BEL or ESC \), such as the window title
// "process-compose: <host>/<directory>" that process-compose sets, and the
// other control strings (DCS, SOS, PM, APC: ESC P, X, ^ or _), which end the
// same way; and the other escape sequences (ESC, intermediate bytes, final
// byte). A control string left unterminated ends with its line, so it cannot
// swallow the rest of the text. Everything else, newlines and tabs included,
// is kept.
func StripTerminalControls(text string) string {
	const escape = 0x1b
	if strings.IndexByte(text, escape) < 0 {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		next := strings.IndexByte(text[i:], escape)
		if next < 0 {
			b.WriteString(text[i:])
			break
		}
		b.WriteString(text[i : i+next])
		i += next + 1
		if i == len(text) {
			break
		}
		switch text[i] {
		case '[':
			i++
			for i < len(text) && text[i] >= 0x20 && text[i] <= 0x3f {
				i++
			}
			if i < len(text) && text[i] >= 0x40 && text[i] <= 0x7e {
				i++
			}
		case ']', 'P', 'X', '^', '_':
			end := strings.IndexAny(text[i:], "\a\x1b\n")
			if end < 0 {
				return b.String()
			}
			i += end
			switch {
			case text[i] == '\a':
				i++
			case text[i] == escape && i+1 < len(text) && text[i+1] == '\\':
				i += 2
			}
		default:
			for i < len(text) && text[i] >= 0x20 && text[i] <= 0x2f {
				i++
			}
			if i < len(text) && text[i] >= 0x30 && text[i] <= 0x7e {
				i++
			}
		}
	}
	return b.String()
}

// LogTruncatedMarker is the first line of a log read LimitLogBytes shortened:
// the lines before it were dropped.
const LogTruncatedMarker = "[truncated]\n"

// LimitLogBytes keeps a log read, labelled as it is delivered, within maxBytes
// (0 or less: no limit). It keeps the last lines in read order, whole: the read
// is cut at a line boundary, and a line that would cross the limit is dropped
// with every line before it. A read it shortens starts with LogTruncatedMarker,
// which counts toward maxBytes. When not even the last line fits beside the
// marker the result is the marker alone, which is longer than a maxBytes below
// its 12 bytes. Limiting the result again changes nothing. Every log read with
// a byte limit goes through it, once, after labelling.
func LimitLogBytes(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	budget := maxBytes - len(LogTruncatedMarker)
	if budget <= 0 {
		return LogTruncatedMarker
	}
	start := len(text) - budget
	if text[start-1] != '\n' {
		cut := strings.IndexByte(text[start:], '\n')
		if cut < 0 {
			return LogTruncatedMarker
		}
		start += cut + 1
	}
	return LogTruncatedMarker + text[start:]
}

// LimitedLogRead collects a log read chunk by chunk for LimitLogBytes, holding
// only the bytes that can reach its result: the last maxBytes+1 (the extra
// byte tells whether the cut falls on a line boundary). Its String is
// LimitLogBytes of everything added.
type LimitedLogRead struct {
	maxBytes int
	buf      []byte
}

// NewLimitedLogRead collects a read to keep within maxBytes (0 or less: no
// limit, and every byte is held).
func NewLimitedLogRead(maxBytes int) *LimitedLogRead {
	return &LimitedLogRead{maxBytes: maxBytes}
}

// Add appends the next chunk of the read, dropping what no longer counts.
func (r *LimitedLogRead) Add(chunk string) {
	if r.maxBytes <= 0 {
		r.buf = append(r.buf, chunk...)
		return
	}
	keep := r.maxBytes + 1
	if len(chunk) >= keep {
		r.buf = append(r.buf[:0], chunk[len(chunk)-keep:]...)
		return
	}
	r.buf = append(r.buf, chunk...)
	// Compact once the buffer holds twice what counts, so each byte is
	// copied a bounded number of times.
	if len(r.buf) > 2*keep {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-keep:]...)
	}
}

// String returns the read so far, kept within maxBytes by LimitLogBytes.
func (r *LimitedLogRead) String() string {
	text := r.buf
	if keep := r.maxBytes + 1; r.maxBytes > 0 && len(text) > keep {
		text = text[len(text)-keep:]
	}
	return LimitLogBytes(string(text), r.maxBytes)
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

// Write accepts native foreground command output through the same bounded log
// owner used by streamed reads. Like Add, callers serialize writes.
func (r *LimitedLogRead) Write(data []byte) (int, error) { r.Add(string(data)); return len(data), nil }
