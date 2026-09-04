package fslock

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	if err := first.Lock(t.Context()); err != nil {
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
