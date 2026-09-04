package fslock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

var lockWaitLogDelay = time.Second

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
	var started time.Time
	if logWait || logHeld {
		started = time.Now()
	}
	waitLogged := false

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := tryLockFile(file)
		if err == nil {
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
			logger.LogAttrs(ctx, slog.LevelInfo, "waiting for lock", slog.String("path", l.path))
			waitLogged = true
		}
		select {
		case <-ctx.Done():
			file.Close()
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
