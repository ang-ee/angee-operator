package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	downErr    error
	// streamLogsExitsEarly makes StreamLogs return an already-closed channel,
	// modelling a follower whose stream ends promptly (a container recreated by
	// `angee restart`, or no containers up yet) so the re-attach loop is
	// exercised without a live process.
	streamLogsExitsEarly bool
}

type applyRecordingBackend struct {
	stubStatusBackend
	applied []runtime.Target
}

func (b *applyRecordingBackend) Apply(_ context.Context, target runtime.Target) error {
	b.applied = append(b.applied, target)
	return nil
}

func TestServiceRestartAppliesPreparedRuntimeConfiguration(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version:        manifest.VersionCurrent,
		Kind:           manifest.KindStack,
		Name:           "restart-config",
		SecretsBackend: manifest.SecretsBackend{Type: "env-file", Path: ".env"},
		Secrets: map[string]manifest.Secret{
			"restart": {Required: true, Import: "env:RESTART_VALUE"},
		},
		Services: map[string]manifest.Service{
			"database": {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"web":      {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}, Env: map[string]string{"RESTART_JOB": "deps", "RESTART_VALUE": "${secret.restart}"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	t.Setenv("RESTART_VALUE", "resolved-value")
	containers := &applyRecordingBackend{}
	local := &applyRecordingBackend{}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	if err := platform.ServiceRestart(context.Background(), []string{"database", "web"}); err != nil {
		t.Fatalf("ServiceRestart: %v", err)
	}
	for name, backend := range map[string]*applyRecordingBackend{"container": containers, "local": local} {
		if len(backend.applied) != 1 {
			t.Fatalf("%s Apply calls = %d, want 1", name, len(backend.applied))
		}
		if len(backend.applied[0].Configuration) == 0 {
			t.Fatalf("%s Apply received empty prepared configuration", name)
		}
	}
	if got := containers.applied[0].Services; len(got) != 1 || got[0] != "database" {
		t.Fatalf("container services = %v, want [database]", got)
	}
	if got := local.applied[0].Services; len(got) != 1 || got[0] != "web" {
		t.Fatalf("local services = %v, want [web]", got)
	}
	if !bytes.Contains(local.applied[0].Configuration, []byte("RESTART_JOB=deps")) {
		t.Fatalf("local configuration does not contain updated environment:\n%s", local.applied[0].Configuration)
	}
	if !bytes.Contains(local.applied[0].Configuration, []byte("RESTART_VALUE=resolved-value")) || bytes.Contains(local.applied[0].Configuration, []byte("${ANGEE_SECRET_RESTART}")) {
		t.Fatalf("local Apply configuration does not contain only the resolved secret:\n%s", local.applied[0].Configuration)
	}
	disk, err := os.ReadFile(filepath.Join(root, "process-compose.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(process-compose.yaml): %v", err)
	}
	if !bytes.Contains(disk, []byte("${ANGEE_SECRET_RESTART}")) || bytes.Contains(disk, []byte("resolved-value")) {
		t.Fatalf("disk process configuration did not retain only the secret placeholder:\n%s", disk)
	}
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
	return b.downErr
}

func TestStackDownUsesGeneratedRuntimeArtifactsAndJoinsErrors(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "down", Services: map[string]manifest.Service{}}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker-compose.yaml", "process-compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("generated"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	composeErr := errors.New("compose down")
	processErr := errors.New("process down")
	containers := &devLifecycleBackend{downErr: composeErr}
	local := &devLifecycleBackend{downErr: processErr}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatal(err)
	}
	err = platform.StackDown(t.Context())
	if !errors.Is(err, composeErr) || !errors.Is(err, processErr) {
		t.Fatalf("StackDown() error = %v, want both backend errors", err)
	}
	if containers.down.Load() != 1 || local.down.Load() != 1 {
		t.Fatalf("down calls = compose %d local %d, want 1 each", containers.down.Load(), local.down.Load())
	}
}

func (b *devLifecycleBackend) StreamLogs(ctx context.Context, _ runtime.LogsRequest) (<-chan string, error) {
	b.logs.Add(1)
	if b.started != nil {
		// Non-blocking: the follower re-attaches, so more calls than the test
		// drains must not wedge this goroutine on a full channel.
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	lines := make(chan string)
	if b.streamLogsExitsEarly {
		close(lines)
		return lines, nil
	}
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

// TestStackDevForegroundFollowerExitDoesNotStopLocalSupervisor proves the dev
// container log follower is detached from the local supervisor's lifetime: when
// the follower's stream ends early (a container recreated by `angee restart`, or
// a transient docker hiccup) it must NOT cancel the local processes, and it must
// re-attach while the dev context is alive. The command still ends cleanly on
// Ctrl-C (ctx cancel) with a nil error.
func TestStackDevForegroundFollowerExitDoesNotStopLocalSupervisor(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "dev-follower-exit",
		Services: map[string]manifest.Service{
			"database": {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"web":      {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	started := make(chan struct{}, 2)
	containers := &devLifecycleBackend{started: started, streamLogsExitsEarly: true}
	local := &devLifecycleBackend{started: started}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- platform.StackDevForeground(ctx, false, io.Discard, io.Discard) }()

	// Both backends must have started: the local supervisor and at least one
	// follower attach (which immediately exits early).
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("dev runtime did not start")
		}
	}

	// The follower has exited early. If its exit tore down the local supervisor,
	// StackDevForeground would return here; assert it stays alive instead.
	select {
	case err := <-done:
		t.Fatalf("StackDevForeground returned early (err=%v): follower exit stopped the local supervisor", err)
	case <-time.After(300 * time.Millisecond):
	}
	if local.foreground.Load() != 1 || local.down.Load() != 0 {
		t.Fatalf("local lifecycle after follower exit: foreground=%d down=%d, want 1/0", local.foreground.Load(), local.down.Load())
	}

	// The follower must re-attach while the context is alive rather than give up
	// after a single stream.
	deadline := time.After(4 * devLogFollowRetryDelay)
	for containers.logs.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("follower did not re-attach: StreamLogs calls = %d, want >= 2", containers.logs.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StackDevForeground: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StackDevForeground did not return after cancellation")
	}
	if local.down.Load() != 0 {
		t.Fatalf("local supervisor was stopped (down=%d), want 0", local.down.Load())
	}
}
