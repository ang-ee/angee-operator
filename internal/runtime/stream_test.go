package runtime

import (
	"context"
	"os/exec"
	"testing"
	"time"
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
