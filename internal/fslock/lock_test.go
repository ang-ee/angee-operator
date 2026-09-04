package fslock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestLockLogsContentionOnlyAtVerboseLevels(t *testing.T) {
	oldDelay := lockWaitLogDelay
	lockWaitLogDelay = 10 * time.Millisecond
	t.Cleanup(func() { lockWaitLogDelay = oldDelay })

	path := filepath.Join(t.TempDir(), "operator.lock")
	first := New(path)
	if err := first.Lock(WithCommand(t.Context(), "angee up")); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	defer first.Unlock()

	var debugLogs bytes.Buffer
	debugCtx, debugCancel := context.WithTimeout(
		logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&debugLogs, slog.LevelDebug))),
		50*time.Millisecond,
	)
	defer debugCancel()
	if err := New(path).Lock(debugCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("debug Lock() error = %v, want deadline exceeded", err)
	}
	got := debugLogs.String()
	if !strings.Contains(got, "waiting for lock path="+path) {
		t.Fatalf("debug trace output = %q, want wait line", got)
	}
	h := readHolder(first.file)
	if !strings.Contains(got, fmt.Sprintf("pid=%d", h.pid)) || !strings.Contains(got, fmt.Sprintf("cmd=%q", h.cmd)) {
		t.Fatalf("debug trace output = %q, want holder pid=%d cmd=%q", got, h.pid, h.cmd)
	}
	if strings.Count(got, "waiting for lock") != 1 {
		t.Fatalf("debug trace output = %q, want one wait line", got)
	}

	var defaultLogs bytes.Buffer
	defaultCtx, defaultCancel := context.WithTimeout(
		logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&defaultLogs, slog.LevelWarn))),
		30*time.Millisecond,
	)
	defer defaultCancel()
	if err := New(path).Lock(defaultCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default Lock() error = %v, want deadline exceeded", err)
	}
	if defaultLogs.Len() != 0 {
		t.Fatalf("default trace output = %q, want none", defaultLogs.String())
	}
}

func TestLockWritesHolderRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.lock")
	lock := New(path)
	ctx := WithCommand(t.Context(), "angee up")
	if err := lock.Lock(ctx); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	defer lock.Unlock()

	h := readHolder(lock.file)
	if h.pid != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", h.pid, os.Getpid())
	}
	if h.cmd != "angee up" {
		t.Fatalf("holder command = %q, want %q", h.cmd, "angee up")
	}
	if _, err := time.Parse(time.RFC3339, h.since); err != nil {
		t.Fatalf("holder since = %q, want RFC3339: %v", h.since, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	want := fmt.Sprintf("pid=%d cmd=%s since=%s\n", h.pid, h.cmd, h.since)
	if string(data) != want {
		t.Fatalf("holder record = %q, want %q", data, want)
	}
}

func TestReadHolderToleratesInvalidRecords(t *testing.T) {
	for _, contents := range []string{"", "garbled", "pid=nope cmd= since=yesterday"} {
		t.Run(contents, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "operator.lock")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer file.Close()
			if h := readHolder(file); h.pid != 0 || h.cmd != "" || h.since != "" {
				t.Fatalf("readHolder() = %+v, want empty holder", h)
			}
		})
	}
}

func TestLockTimeoutNamesHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.lock")
	first := New(path)
	if err := first.Lock(t.Context()); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	defer first.Unlock()
	h := readHolder(first.file)

	t.Setenv("ANGEE_LOCK_TIMEOUT", "30ms")
	err := New(path).Lock(t.Context())
	want := fmt.Sprintf("%s held by pid %d (%s) since %s; waited 30ms (ANGEE_LOCK_TIMEOUT)", path, h.pid, h.cmd, h.since)
	if err == nil || err.Error() != want {
		t.Fatalf("second Lock() error = %v, want %q", err, want)
	}
}

func TestLockLogsAcquisitionAndReleaseAtDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.lock")
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	lock := New(path)
	if err := lock.Lock(ctx); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "lock acquired path="+path+" wait=") ||
		!strings.Contains(got, "lock released path="+path+" held=") {
		t.Fatalf("trace output = %q", got)
	}
}

func TestLockContentionHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.lock")
	first := New(path)
	if err := first.Lock(context.Background()); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	defer first.Unlock()

	second := New(path)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := second.Lock(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Lock() error = %v, want deadline exceeded", err)
	}
}

func TestLockReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.lock")
	first := New(path)
	if err := first.Lock(context.Background()); err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	second := New(path)
	if err := second.Lock(context.Background()); err != nil {
		t.Fatalf("second Lock() error = %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatalf("second Unlock() error = %v", err)
	}
}
