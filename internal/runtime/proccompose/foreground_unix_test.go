//go:build !windows

package proccompose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

func TestTransientJobKillsAnINTIgnoringProcessGroupAfterTheWaitDelay(t *testing.T) {
	root := t.TempDir()
	parentPID := filepath.Join(root, "parent.pid")
	childPID := filepath.Join(root, "child.pid")
	readyFIFO := filepath.Join(root, "ready")
	if err := syscall.Mkfifo(readyFIFO, 0600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyFIFO, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	binary := filepath.Join(root, "fake-process-compose")
	script := fmt.Sprintf(`#!/bin/sh
trap '' INT TERM
sh -c 'trap "" INT TERM; while :; do sleep 30; done' &
echo $$ > %q
echo $! > %q
printf ready > %q
wait
`, parentPID, childPID, readyFIFO)
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	configuration, err := Marshal(File{Version: "0.5", Processes: map[string]Process{"capture": {
		Command: "unused", Shutdown: &Shutdown{TimeoutSeconds: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, runErr := (Backend{LookupPath: func(string) (string, error) { return binary, nil }}).RunJob(ctx, runtime.Target{Root: root}, runtime.JobSpec{
			Name: "capture", Configuration: configuration, Transient: true,
		})
		finished <- runErr
	}()
	readyResult := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(ready, make([]byte, len("ready")))
		readyResult <- readErr
	}()
	select {
	case err := <-readyResult:
		if err != nil {
			t.Fatalf("wait for fake process-compose readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = ready.Close()
		t.Fatal("fake process-compose did not report readiness")
	}
	started := time.Now()
	cancel()
	select {
	case err = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJob() did not return after cancellation")
	}
	elapsed := time.Since(started)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunJob() error = %v, want context cancellation", err)
	}
	if elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Fatalf("RunJob() returned after %s, want the 2s interrupt grace before group kill", elapsed)
	}
	assertUnixProcessGone(t, parentPID)
	assertUnixProcessGone(t, childPID)
}

func assertUnixProcessGone(t *testing.T, path string) {
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
