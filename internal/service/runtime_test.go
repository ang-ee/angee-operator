package service

import (
	"bytes"
	"os"
	"sync"
	"testing"
)

// TestGuardDevSink keeps the colouring contract: a real terminal (*os.File) is
// passed through so exec hands the child the TTY fd, while any other sink is
// wrapped for safe concurrent writes.
func TestGuardDevSink(t *testing.T) {
	if got := guardDevSink(os.Stdout); got != os.Stdout {
		t.Fatalf("guardDevSink(*os.File) = %T, want the file unwrapped", got)
	}
	var buf bytes.Buffer
	if _, ok := guardDevSink(&buf).(*syncWriter); !ok {
		t.Fatalf("guardDevSink(non-file) did not wrap in *syncWriter")
	}
}

// TestSyncWriterSerializesConcurrentWrites guards the dev-stream race fix: the
// two `angee dev` backends write to the same sink concurrently, so syncWriter
// must serialize those writes. The underlying bytes.Buffer is not safe for
// concurrent use, so `go test -race` fails here if the mutex is ever removed.
func TestSyncWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := &syncWriter{w: &buf}

	const writers, perWriter = 8, 100
	line := []byte("agent-demo-agent | starting\n")
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				if _, err := w.Write(line); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got, want := buf.Len(), writers*perWriter*len(line); got != want {
		t.Fatalf("buffered %d bytes, want %d", got, want)
	}
}
