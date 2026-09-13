package service

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
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

type devLifecycleBackend struct {
	stubStatusBackend
	up         atomic.Int32
	foreground atomic.Int32
	down       atomic.Int32
	logs       atomic.Int32
	started    chan struct{}
}

func (b *devLifecycleBackend) Up(context.Context, runtime.Target) error {
	b.up.Add(1)
	return nil
}

func (b *devLifecycleBackend) UpForeground(ctx context.Context, _ runtime.Target, _, _ io.Writer) error {
	b.foreground.Add(1)
	if b.started != nil {
		b.started <- struct{}{}
	}
	<-ctx.Done()
	return nil
}

func (b *devLifecycleBackend) Down(context.Context, runtime.Target) error {
	b.down.Add(1)
	return nil
}

func (b *devLifecycleBackend) StreamLogs(ctx context.Context, _ runtime.LogsRequest) (<-chan string, error) {
	b.logs.Add(1)
	if b.started != nil {
		b.started <- struct{}{}
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		<-ctx.Done()
	}()
	return lines, nil
}

func TestStackDevForegroundLeavesContainersRunningOnCancellation(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "dev-lifecycle",
		Services: map[string]manifest.Service{
			"database": {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"web":      {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	started := make(chan struct{}, 2)
	containers := &devLifecycleBackend{started: started}
	local := &devLifecycleBackend{started: started}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- platform.StackDevForeground(ctx, false, io.Discard, io.Discard) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("dev runtime did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StackDevForeground: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StackDevForeground did not return after cancellation")
	}
	if containers.up.Load() != 1 || containers.logs.Load() != 1 || containers.foreground.Load() != 0 || containers.down.Load() != 0 {
		t.Fatalf("container lifecycle: up=%d logs=%d foreground=%d down=%d", containers.up.Load(), containers.logs.Load(), containers.foreground.Load(), containers.down.Load())
	}
	if local.foreground.Load() != 1 || local.down.Load() != 0 {
		t.Fatalf("local lifecycle: foreground=%d down=%d", local.foreground.Load(), local.down.Load())
	}
}
