package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func collect(t *testing.T, ch <-chan string, want int) []string {
	t.Helper()
	var got []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, line)
			if want > 0 && len(got) > want {
				t.Fatalf("received more than %d lines: %q", want, got)
			}
		case <-timeout:
			t.Fatalf("timed out after %d lines: %q", len(got), got)
		}
	}
}

func TestReplayLinesFramesPerLine(t *testing.T) {
	ch := ReplayLines(context.Background(), []byte("alpha\nbeta\ngamma\n"))
	got := collect(t, ch, 3)
	want := []string{"alpha\n", "beta\n", "gamma\n"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %q", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReplayLinesStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := ReplayLines(ctx, []byte("one\ntwo\n"))
	// A cancelled context means the producer may emit nothing; the channel
	// must still close rather than hang.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("channel did not close after context cancel")
	}
}

func TestStreamCommandStreamsLinesThenCloses(t *testing.T) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'a\\nb\\nc\\n'")
	ch, err := StreamCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("StreamCommand: %v", err)
	}
	got := collect(t, ch, 3)
	if len(got) != 3 || got[0] != "a\n" || got[2] != "c\n" {
		t.Fatalf("got %q, want a/b/c", got)
	}
}

func TestStreamCommandTracesCommandWithRedactedArgs(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'ok\\n'", "--token", "secret", "https://user:password@example.com/repo")
	ch, err := StreamCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("StreamCommand() error = %v", err)
	}
	if got := collect(t, ch, 1); len(got) != 1 || got[0] != "ok\n" {
		t.Fatalf("streamed output = %q, want ok", got)
	}
	got := logs.String()
	if !strings.Contains(got, "exec sh -c") ||
		!strings.Contains(got, "--token *** https://***@example.com/repo") ||
		!strings.Contains(got, "exec finished duration=") {
		t.Fatalf("trace output = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "password") || strings.Contains(got, "user") {
		t.Fatalf("trace output leaked secret data: %q", got)
	}
}

func TestStreamCommandTearsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A never-exiting follow-like process: emit a line, then block.
	cmd := exec.CommandContext(ctx, "sh", "-c", "echo started; sleep 30")
	ch, err := StreamCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("StreamCommand: %v", err)
	}
	if line, ok := <-ch; !ok || line != "started\n" {
		t.Fatalf("first line = %q ok=%v, want started", line, ok)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any trailing buffered line, then it must close.
			if _, ok := <-ch; ok {
				t.Fatal("channel still open after cancel")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not close after cancel killed the process")
	}
}

func TestPrefixLines(t *testing.T) {
	tests := []struct {
		text, label string
		width       int
		want        string
	}{
		{"a\nb\n", "api", 6, "api    | a\napi    | b\n"},
		{"unterminated", "worker", 6, "worker | unterminated"},
		{"", "api", 6, ""},
		{"x\n", "longer", 3, "longer | x\n"},
	}
	for _, tt := range tests {
		if got := PrefixLines(tt.text, tt.label, tt.width); got != tt.want {
			t.Errorf("PrefixLines(%q, %q, %d) = %q, want %q", tt.text, tt.label, tt.width, got, tt.want)
		}
	}
}

// A byte limit keeps the newest whole lines, cut at a line boundary, behind a
// leading marker that counts toward the limit (#118). The result stays within
// the limit and a second pass leaves it unchanged.
func TestLimitLogBytesKeepsTheNewestWholeLines(t *testing.T) {
	const labelled = "api    | 1\napi    | 2\nworker | 3\nworker | 4\n" // four 11-byte lines
	tests := []struct {
		text     string
		maxBytes int
		want     string
	}{
		{labelled, 0, labelled},
		{labelled, -1, labelled},
		{labelled, len(labelled), labelled},
		// 40 bytes leave 28 for lines: the third line's start is the first
		// boundary inside them.
		{labelled, 40, "[truncated]\nworker | 3\nworker | 4\n"},
		// A cut that falls on a boundary keeps the line after it.
		{labelled, 34, "[truncated]\nworker | 3\nworker | 4\n"},
		{labelled, 33, "[truncated]\nworker | 4\n"},
		// The newest line alone is longer than the room left: nothing whole fits.
		{labelled, 20, "[truncated]\n"},
		// An unterminated newest line is a line too.
		{"line-1\nline-2\npartial", 20, "[truncated]\npartial"},
		{"line-1\nlongpartialline", 15, "[truncated]\n"},
		{"", 0, ""},
		{"", 5, ""},
		// Multi-byte UTF-8: "ï" is two bytes. A cut that falls inside a
		// rune moves to the next line, so no rune is split.
		{"naïve-1\nnaïve-2\nnaïve-3\n", 21, "[truncated]\nnaïve-3\n"},
		{"ïïïïïïïï\nok\n", 17, "[truncated]\nok\n"},
		// CRLF: "\r" stays with its line, even when the cut falls between
		// "\r" and "\n".
		{"one\r\ntwo\r\nsix\r\nten\r\n", 17, "[truncated]\nten\r\n"},
		{"one\r\ntwo\r\nsix\r\nten\r\n", 18, "[truncated]\nten\r\n"},
	}
	for _, tt := range tests {
		got := LimitLogBytes(tt.text, tt.maxBytes)
		if got != tt.want {
			t.Errorf("LimitLogBytes(%q, %d) = %q, want %q", tt.text, tt.maxBytes, got, tt.want)
		}
		if tt.maxBytes > 0 && len(got) > tt.maxBytes {
			t.Errorf("LimitLogBytes(%q, %d) is %d bytes, over the limit", tt.text, tt.maxBytes, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("LimitLogBytes(%q, %d) = %q splits a rune", tt.text, tt.maxBytes, got)
		}
		if again := LimitLogBytes(got, tt.maxBytes); again != got {
			t.Errorf("LimitLogBytes(%q, %d) again = %q, want it unchanged", got, tt.maxBytes, again)
		}
	}
	// A limit up to the marker's 12 bytes has no room for a line: a read it
	// shortens is the marker alone, over the limit when the limit is below 12.
	for maxBytes := 1; maxBytes <= len(LogTruncatedMarker); maxBytes++ {
		if got := LimitLogBytes(labelled, maxBytes); got != LogTruncatedMarker {
			t.Errorf("LimitLogBytes(%q, %d) = %q, want the marker alone", labelled, maxBytes, got)
		}
	}
}

// Collecting a read chunk by chunk gives what LimitLogBytes gives the joined
// read, for every limit and every way of cutting the read into chunks, while
// holding at most twice the bytes that count.
func TestLimitedLogReadMatchesLimitLogBytes(t *testing.T) {
	var whole strings.Builder
	for i := range 60 {
		whole.WriteString(strings.Repeat("x", i%7) + "line-" + strconv.Itoa(i) + "\n")
	}
	whole.WriteString("unterminated")
	text := whole.String()
	for _, maxBytes := range []int{0, 1, 11, 12, 13, 20, 37, 64, 100, 333, len(text) - 1, len(text), len(text) + 1} {
		for _, size := range []int{1, 2, 7, 13, 50, len(text)} {
			read := NewLimitedLogRead(maxBytes)
			for start := 0; start < len(text); start += size {
				read.Add(text[start:min(start+size, len(text))])
				if maxBytes > 0 && len(read.buf) > 2*(maxBytes+1) {
					t.Fatalf("limit %d, chunks of %d: holding %d bytes, want at most %d", maxBytes, size, len(read.buf), 2*(maxBytes+1))
				}
			}
			if got, want := read.String(), LimitLogBytes(text, maxBytes); got != want {
				t.Fatalf("limit %d, chunks of %d: read = %q, want %q", maxBytes, size, got, want)
			}
		}
	}
}

func TestMergeStreamsDeliversEveryLineAndCloses(t *testing.T) {
	a, b := make(chan string, 2), make(chan string, 1)
	a <- "a1\n"
	a <- "a2\n"
	b <- "b1\n"
	close(a)
	close(b)
	got := collect(t, MergeStreams(t.Context(), a, b), 3)
	slices.Sort(got)
	if want := []string{"a1\n", "a2\n", "b1\n"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %q, want %q", got, want)
	}
}
