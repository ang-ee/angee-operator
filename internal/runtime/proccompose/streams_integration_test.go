//go:build !windows

package proccompose

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

const localStreamIntegrationPayloadSize = 16 << 20

func TestStreamJobsUseStdinAndFD3WithoutLeavingProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process-compose stream integration")
	}
	if err := transientJobSupport(); err != nil {
		t.Skip(err)
	}
	requireProcessComposeIntegration(t)
	root := t.TempDir()
	payload := make([]byte, localStreamIntegrationPayloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(root, "payload")
	if err := os.WriteFile(payloadPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	backend := NewBackend()
	target := runtime.Target{Root: root}

	t.Run("fd3 save is byte identical", func(t *testing.T) {
		job := localStreamJob(t, "capture", "cat "+strconv.Quote(payloadPath))
		var artifact bytes.Buffer
		diagnostics, err := backend.RunJobStreams(t.Context(), target, job, runtime.JobStreams{Role: runtime.StreamRoleSave, Stdout: &artifact})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := sha256.Sum256(artifact.Bytes()), sha256.Sum256(payload); got != want {
			t.Fatalf("artifact sha256 = %x, want %x", got, want)
		}
		if len(diagnostics) >= 64<<10 {
			t.Fatalf("diagnostics = %d bytes, want a small native-output tail", len(diagnostics))
		}
	})

	t.Run("stdin passthrough", func(t *testing.T) {
		received := filepath.Join(root, "received")
		job := localStreamJob(t, "restore", "cat > "+strconv.Quote(received))
		_, err := backend.RunJobStreams(t.Context(), target, job, runtime.JobStreams{Role: runtime.StreamRoleRestore, Stdin: bytes.NewReader(payload)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(received)
		if err != nil {
			t.Fatal(err)
		}
		if gotHash, wantHash := sha256.Sum256(got), sha256.Sum256(payload); gotHash != wantHash {
			t.Fatalf("received sha256 = %x, want %x", gotHash, wantHash)
		}
	})

	t.Run("producer failure", func(t *testing.T) {
		job := localStreamJob(t, "capture", "head -c 1048576 "+strconv.Quote(payloadPath)+"; exit 23")
		var artifact bytes.Buffer
		_, err := backend.RunJobStreams(t.Context(), target, job, runtime.JobStreams{Role: runtime.StreamRoleSave, Stdout: &artifact})
		if err == nil {
			t.Fatal("producer failure returned success")
		}
		if artifact.Len() != 1<<20 {
			t.Fatalf("partial artifact = %d bytes, want 1 MiB", artifact.Len())
		}
	})

	t.Run("consumer death kills the group", func(t *testing.T) {
		pidFile := filepath.Join(root, "consumer-death.pid")
		command := fmt.Sprintf("echo $$$$ > %s; trap '' INT TERM; while :; do cat %s; done", strconv.Quote(pidFile), strconv.Quote(payloadPath))
		job := localStreamJob(t, "capture", command)
		consumerErr := errors.New("consumer closed")
		_, err := backend.RunJobStreams(t.Context(), target, job, runtime.JobStreams{
			Role: runtime.StreamRoleSave, Stdout: &localErrorAfterWriter{remaining: 1 << 20, err: consumerErr},
		})
		if !errors.Is(err, consumerErr) {
			t.Fatalf("consumer failure = %v, want %v", err, consumerErr)
		}
		assertPIDGone(t, pidFile)
	})

	t.Run("cancel kills the group", func(t *testing.T) {
		pidFile := filepath.Join(root, "cancel.pid")
		command := fmt.Sprintf("echo $$$$ > %s; trap '' INT TERM; while :; do cat %s; done", strconv.Quote(pidFile), strconv.Quote(payloadPath))
		job := localStreamJob(t, "capture", command)
		ctx, cancel := context.WithCancel(t.Context())
		started := make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			_, err := backend.RunJobStreams(ctx, target, job, runtime.JobStreams{Role: runtime.StreamRoleSave, Stdout: &localNotifyingWriter{started: started}})
			finished <- err
		}()
		select {
		case <-started:
			cancel()
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatal("local producer did not start")
		}
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("canceled local stream job did not return")
		}
		assertPIDGone(t, pidFile)
	})
}

func localStreamJob(t *testing.T, name, command string) runtime.JobSpec {
	t.Helper()
	configuration, err := Marshal(File{Version: "0.5", Processes: map[string]Process{
		name: {Command: command, Shutdown: &Shutdown{TimeoutSeconds: 1, Signal: int(syscall.SIGTERM)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return runtime.JobSpec{Name: name, Configuration: configuration, Transient: true}
}

type localErrorAfterWriter struct {
	remaining int
	err       error
}

func (w *localErrorAfterWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	n := min(len(data), w.remaining)
	w.remaining -= n
	if n != len(data) {
		return n, w.err
	}
	return n, nil
}

type localNotifyingWriter struct {
	once    sync.Once
	started chan struct{}
}

func (w *localNotifyingWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return len(data), nil
}

func assertPIDGone(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		t.Fatalf("process %d from %s still exists", pid, path)
	}
}
