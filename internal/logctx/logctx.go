// Package logctx carries structured loggers through contexts and provides
// terminal-oriented logging helpers shared by Angee command surfaces.
package logctx

import (
	"bytes"
	"context"
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type loggerKey struct{}

var discardLogger = slog.New(slog.DiscardHandler)

var (
	stepFirstInfoDelay = 3 * time.Second
	stepInfoInterval   = 5 * time.Second
	stepWarnDelay      = 30 * time.Second
)

// With returns a child context carrying logger. A nil logger is replaced by a
// discard logger so callers of From never need to check for nil.
func With(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		logger = discardLogger
	}
	return context.WithValue(ctx, loggerKey{}, logger)
}

// From returns the logger stored in ctx, or a discard logger when ctx does not
// carry one.
func From(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return discardLogger
}

// LevelFromCount maps a verbosity count to the corresponding logging level.
func LevelFromCount(n int) slog.Level {
	switch {
	case n <= 0:
		return slog.LevelWarn
	case n == 1:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// CountFromEnv reads a non-negative verbosity count from the named environment
// variable. Empty and zero values select the default level, values above two
// are clamped to two, and invalid values report ok as false.
func CountFromEnv(name string) (count int, ok bool) {
	value, exists := os.LookupEnv(name)
	if !exists {
		return 0, false
	}
	if value == "" {
		return 0, true
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return 0, false
	}
	return min(count, 2), true
}

// NewCLIHandler returns a structured log handler for terminal diagnostics.
// Warnings and errors use "warning:" and "error:" prefixes; info and debug
// records use "angee:". At debug verbosity every line also carries elapsed
// time since the handler was created.
func NewCLIHandler(w io.Writer, level slog.Level) slog.Handler {
	return &cliHandler{
		state: &cliHandlerState{
			writer:  w,
			created: time.Now(),
		},
		level: level,
	}
}

type cliHandlerState struct {
	mu      sync.Mutex
	writer  io.Writer
	created time.Time
}

type storedAttr struct {
	groups []string
	attr   slog.Attr
}

type cliHandler struct {
	state  *cliHandlerState
	level  slog.Level
	attrs  []storedAttr
	groups []string
}

func (h *cliHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *cliHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Level < h.level {
		return nil
	}

	h.state.mu.Lock()
	defer h.state.mu.Unlock()

	var line bytes.Buffer
	if h.level == slog.LevelDebug {
		elapsed := time.Since(h.state.created).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		fmt.Fprintf(&line, "[+%.3fs] ", elapsed)
	}
	switch {
	case record.Level >= slog.LevelError:
		line.WriteString("error: ")
	case record.Level >= slog.LevelWarn:
		line.WriteString("warning: ")
	default:
		line.WriteString("angee: ")
	}
	line.WriteString(record.Message)

	for _, stored := range h.attrs {
		appendAttr(&line, stored.groups, stored.attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(&line, h.groups, attr)
		return true
	})
	line.WriteByte('\n')
	_, err := h.state.writer.Write(line.Bytes())
	return err
}

func (h *cliHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append([]storedAttr(nil), h.attrs...)
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, storedAttr{
			groups: append([]string(nil), h.groups...),
			attr:   attr,
		})
	}
	return &clone
}

func (h *cliHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func appendAttr(line *bytes.Buffer, groups []string, attr slog.Attr) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			groups = append(append([]string(nil), groups...), attr.Key)
		}
		for _, child := range value.Group() {
			appendAttr(line, groups, child)
		}
		return
	}

	keyParts := make([]string, 0, len(groups)+1)
	keyParts = append(keyParts, groups...)
	keyParts = append(keyParts, attr.Key)
	line.WriteByte(' ')
	appendText(line, strings.Join(keyParts, "."))
	line.WriteByte('=')
	appendValue(line, value)
}

func appendValue(line *bytes.Buffer, value slog.Value) {
	switch value.Kind() {
	case slog.KindString:
		appendText(line, value.String())
	case slog.KindTime:
		line.WriteString(value.Time().Format(time.RFC3339Nano))
	case slog.KindAny:
		if marshaler, ok := value.Any().(encoding.TextMarshaler); ok {
			text, err := marshaler.MarshalText()
			if err != nil {
				appendText(line, "!ERROR:"+err.Error())
				return
			}
			appendText(line, string(text))
			return
		}
		if data, ok := value.Any().([]byte); ok {
			line.WriteString(strconv.Quote(string(data)))
			return
		}
		appendText(line, fmt.Sprintf("%+v", value.Any()))
	default:
		line.WriteString(value.String())
	}
}

func appendText(line *bytes.Buffer, value string) {
	if needsQuoting(value) {
		line.WriteString(strconv.Quote(value))
		return
	}
	line.WriteString(value)
}

func needsQuoting(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if r == '=' || r == '"' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}

// RedactURL replaces URL user information with a fixed placeholder. Invalid
// URLs and URLs without user information are returned unchanged.
func RedactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User == nil {
		return rawURL
	}
	parsed.User = url.User("***")
	return parsed.String()
}

// RedactArgs returns a copy of args with common secret flag values and URL
// user information replaced by fixed placeholders.
func RedactArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for i := 0; i < len(redacted); i++ {
		redacted[i] = RedactURL(redacted[i])
		for _, flag := range []string{"--token", "--password", "--jwt-secret"} {
			if redacted[i] == flag {
				if i+1 < len(redacted) {
					redacted[i+1] = "***"
					i++
				}
				break
			}
			if strings.HasPrefix(redacted[i], flag+"=") {
				redacted[i] = flag + "=***"
				break
			}
		}
	}
	return redacted
}

// EnvKeys returns the keys from KEY=value environment entries, preserving
// their input order and omitting malformed entries.
func EnvKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// Truncate returns output capped to max captured bytes. When bytes are removed,
// the returned string includes the number of truncated bytes.
func Truncate(output []byte, maxBytes int) string {
	maxBytes = max(maxBytes, 0)
	if len(output) <= maxBytes {
		return string(output)
	}
	return fmt.Sprintf("%s … (%d bytes truncated)", output[:maxBytes], len(output)-maxBytes)
}

// Step logs the start, heartbeat, and completion of a potentially slow unit
// of work. The returned completion function is safe to call more than once.
func Step(ctx context.Context, msg string, attrs ...slog.Attr) func(err error) {
	complete, _ := startStep(ctx, msg, attrs...)
	return complete
}

func startStep(ctx context.Context, msg string, attrs ...slog.Attr) (func(error), <-chan struct{}) {
	logger := From(ctx)
	started := time.Now()
	stepAttrs := append([]slog.Attr(nil), attrs...)
	logger.LogAttrs(ctx, slog.LevelDebug, msg, stepAttrs...)

	stop := make(chan struct{})
	stopped := make(chan struct{})
	firstInfoDelay := stepFirstInfoDelay
	infoInterval := stepInfoInterval
	warnDelay := stepWarnDelay
	infoEnabled := logger.Enabled(ctx, slog.LevelInfo)
	go func() {
		defer close(stopped)
		if infoEnabled {
			runInfoHeartbeat(ctx, logger, msg, stepAttrs, stop, firstInfoDelay, infoInterval)
			return
		}
		runWarnHeartbeat(ctx, logger, msg, stepAttrs, stop, warnDelay)
	}()

	var once sync.Once
	complete := func(err error) {
		once.Do(func() {
			close(stop)
			<-stopped
			completionAttrs := make([]slog.Attr, 0, len(stepAttrs)+2)
			completionAttrs = append(completionAttrs, stepAttrs...)
			completionAttrs = append(completionAttrs, slog.Duration("duration", time.Since(started)))
			level := slog.LevelDebug
			if err != nil {
				level = slog.LevelWarn
				completionAttrs = append(completionAttrs, slog.Any("err", err))
			}
			logger.LogAttrs(ctx, level, "finished "+msg, completionAttrs...)
		})
	}
	return complete, stopped
}

func runInfoHeartbeat(ctx context.Context, logger *slog.Logger, msg string, attrs []slog.Attr, stop <-chan struct{}, firstDelay, interval time.Duration) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	running := firstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
			logger.LogAttrs(ctx, slog.LevelInfo, fmt.Sprintf("still %s (%s)", msg, running), attrs...)
			running += interval
			timer.Reset(interval)
		}
	}
}

func runWarnHeartbeat(ctx context.Context, logger *slog.Logger, msg string, attrs []slog.Attr, stop <-chan struct{}, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-stop:
	case <-timer.C:
		logger.LogAttrs(ctx, slog.LevelWarn, fmt.Sprintf("still %s after %s", msg, delay), attrs...)
	}
}
