package fslock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

const lockTimeoutEnv = "ANGEE_LOCK_TIMEOUT"

var lockWaitLogDelay = time.Second

type commandContextKey struct{}

type holder struct {
	pid   int
	cmd   string
	since string
}

type Lock struct {
	path       string
	file       *os.File
	traceCtx   context.Context
	logger     *slog.Logger
	acquiredAt time.Time
}

func New(path string) *Lock {
	return &Lock{path: path}
}

func RootLock(root string) *Lock {
	return New(filepath.Join(root, "run", "operator.lock"))
}

// WithCommand records the resolved command path to write when a lock is
// acquired. Callers that do not provide one fall back to argv[0], plus argv[1]
// when it is not a flag.
func WithCommand(ctx context.Context, command string) context.Context {
	command = strings.Join(strings.Fields(command), " ")
	if command == "" {
		return ctx
	}
	return context.WithValue(ctx, commandContextKey{}, command)
}

func (l *Lock) Lock(ctx context.Context) error {
	if l.file != nil {
		return errors.New("lock is already held")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	logger := logctx.From(ctx)
	logWait := logger.Enabled(ctx, slog.LevelInfo)
	logHeld := logger.Enabled(ctx, slog.LevelDebug)
	timeout, err := lockTimeout()
	if err != nil {
		_ = file.Close()
		return err
	}
	started := time.Now()
	waitLogged := false

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := tryLockFile(file)
		if err == nil {
			if err := writeHolder(file, currentHolder(ctx)); err != nil {
				_ = unlockFile(file)
				_ = file.Close()
				return fmt.Errorf("record lock holder in %s: %w", l.path, err)
			}
			l.file = file
			if logHeld {
				acquiredAt := time.Now()
				l.traceCtx = ctx
				l.logger = logger
				l.acquiredAt = acquiredAt
				logger.LogAttrs(ctx, slog.LevelDebug, "lock acquired",
					slog.String("path", l.path),
					slog.Duration("wait", acquiredAt.Sub(started)),
				)
			}
			return nil
		}
		if !isLockBusy(err) {
			file.Close()
			return fmt.Errorf("lock %s: %w", l.path, err)
		}
		if logWait && !waitLogged && time.Since(started) >= lockWaitLogDelay {
			logWaiting(ctx, logger, l.path, readHolder(file))
			waitLogged = true
		}
		if timeout > 0 && time.Since(started) >= timeout {
			h := readHolder(file)
			_ = file.Close()
			return lockTimeoutError(l.path, h, timeout)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func lockTimeout() (time.Duration, error) {
	raw, ok := os.LookupEnv(lockTimeoutEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < 0 {
		return 0, fmt.Errorf("invalid %s %q: expected a non-negative Go duration", lockTimeoutEnv, raw)
	}
	return timeout, nil
}

func currentHolder(ctx context.Context) holder {
	command, _ := ctx.Value(commandContextKey{}).(string)
	if command == "" {
		command = filepath.Base(os.Args[0])
		if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
			command += " " + os.Args[1]
		}
	}
	command = strings.Join(strings.Fields(command), " ")
	return holder{pid: os.Getpid(), cmd: command, since: time.Now().UTC().Format(time.RFC3339)}
}

func writeHolder(file *os.File, h holder) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	// No fsync: the record is only read back by same-host waiters from the
	// page cache, so durability would add latency to every lock for nothing.
	_, err := fmt.Fprintf(file, "pid=%d cmd=%s since=%s\n", h.pid, h.cmd, h.since)
	return err
}

// readHolder returns the holder record another process wrote after locking.
// On Windows the exclusive LockFileEx range makes this read fail, so the
// diagnostics stay empty there and only the wait itself is reported.
func readHolder(file *os.File) holder {
	if file == nil {
		return holder{}
	}
	data := make([]byte, 4096)
	n, err := file.ReadAt(data, 0)
	if err != nil && n == 0 {
		return holder{}
	}
	text := strings.TrimSpace(string(data[:n]))
	var h holder
	if strings.HasPrefix(text, "pid=") {
		pidText := strings.TrimPrefix(strings.SplitN(text, " ", 2)[0], "pid=")
		if pid, err := strconv.Atoi(pidText); err == nil && pid > 0 {
			h.pid = pid
		}
	}
	cmdStart := strings.Index(text, " cmd=")
	sinceStart := strings.LastIndex(text, " since=")
	if cmdStart >= 0 {
		start := cmdStart + len(" cmd=")
		end := len(text)
		if sinceStart >= start {
			end = sinceStart
		}
		h.cmd = strings.TrimSpace(text[start:end])
	}
	if sinceStart >= 0 {
		since := strings.TrimSpace(text[sinceStart+len(" since="):])
		if _, err := time.Parse(time.RFC3339, since); err == nil {
			h.since = since
		}
	}
	return h
}

func logWaiting(ctx context.Context, logger *slog.Logger, path string, h holder) {
	attrs := []slog.Attr{slog.String("path", path)}
	if h.pid > 0 {
		attrs = append(attrs, slog.Int("pid", h.pid))
	}
	if h.cmd != "" {
		attrs = append(attrs, slog.String("cmd", h.cmd))
	}
	if h.since != "" {
		attrs = append(attrs, slog.String("since", h.since))
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "waiting for lock", attrs...)
}

func lockTimeoutError(path string, h holder, timeout time.Duration) error {
	suffix := fmt.Sprintf("; waited %s (%s)", timeout, lockTimeoutEnv)
	if h.pid > 0 && h.cmd != "" && h.since != "" {
		return fmt.Errorf("%s held by pid %d (%s) since %s%s", path, h.pid, h.cmd, h.since, suffix)
	}
	if h.pid > 0 && h.cmd != "" {
		return fmt.Errorf("%s held by pid %d (%s)%s", path, h.pid, h.cmd, suffix)
	}
	if h.pid > 0 {
		return fmt.Errorf("%s held by pid %d%s", path, h.pid, suffix)
	}
	return fmt.Errorf("%s is held%s", path, suffix)
}

func (l *Lock) Unlock() error {
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	traceCtx := l.traceCtx
	logger := l.logger
	acquiredAt := l.acquiredAt
	l.traceCtx = nil
	l.logger = nil
	l.acquiredAt = time.Time{}
	if err := unlockFile(file); err != nil {
		_ = file.Close()
		return err
	}
	err := file.Close()
	if logger != nil {
		logger.LogAttrs(traceCtx, slog.LevelDebug, "lock released",
			slog.String("path", l.path),
			slog.Duration("held", time.Since(acquiredAt)),
		)
	}
	return err
}

func (l *Lock) With(ctx context.Context, fn func() error) (err error) {
	if err := l.Lock(ctx); err != nil {
		return err
	}
	defer func() {
		if unlockErr := l.Unlock(); err == nil && unlockErr != nil {
			err = unlockErr
		}
	}()
	return fn()
}
