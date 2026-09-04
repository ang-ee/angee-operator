package logctx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestLevelFromCount(t *testing.T) {
	tests := []struct {
		name  string
		count int
		want  slog.Level
	}{
		{name: "negative", count: -1, want: slog.LevelWarn},
		{name: "default", count: 0, want: slog.LevelWarn},
		{name: "verbose", count: 1, want: slog.LevelInfo},
		{name: "very verbose", count: 2, want: slog.LevelDebug},
		{name: "clamped", count: 8, want: slog.LevelDebug},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := LevelFromCount(test.count); got != test.want {
				t.Fatalf("LevelFromCount(%d) = %v, want %v", test.count, got, test.want)
			}
		})
	}
}

func TestCountFromEnv(t *testing.T) {
	const missingName = "ANGEE_LOGCTX_TEST_MISSING"
	original, existed := os.LookupEnv(missingName)
	if err := os.Unsetenv(missingName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(missingName, original)
		} else {
			_ = os.Unsetenv(missingName)
		}
	})
	if got, ok := CountFromEnv(missingName); got != 0 || ok {
		t.Fatalf("CountFromEnv(unset) = (%d, %t), want (0, false)", got, ok)
	}

	const name = "ANGEE_LOGCTX_TEST_VERBOSE"
	tests := []struct {
		value string
		want  int
		ok    bool
	}{
		{value: "", want: 0, ok: true},
		{value: "0", want: 0, ok: true},
		{value: "1", want: 1, ok: true},
		{value: "2", want: 2, ok: true},
		{value: "9", want: 2, ok: true},
		{value: "-1", want: 0, ok: false},
		{value: "debug", want: 0, ok: false},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(name, test.value)
			got, ok := CountFromEnv(name)
			if got != test.want || ok != test.ok {
				t.Fatalf("CountFromEnv(%q) = (%d, %t), want (%d, %t)", test.value, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestCLIHandlerRendering(t *testing.T) {
	t.Run("warn", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelWarn))
		logger.Info("hidden")
		logger.Warn("source refresh failed", "source", "django")
		logger.Error("operation failed", "attempt", 2)

		want := "warning: source refresh failed source=django\n" +
			"error: operation failed attempt=2\n"
		if got := output.String(); got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})

	t.Run("info", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelInfo))
		logger.Debug("hidden")
		logger.Info("refreshing source", "source", "django")

		if got, want := output.String(), "angee: refreshing source source=django\n"; got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})

	t.Run("debug elapsed prefix", func(t *testing.T) {
		var output bytes.Buffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelDebug))
		logger.Debug("running command", "argv", "git fetch")
		logger.Warn("slow command")

		pattern := `(?m)^\[\+\d+\.\d{3}s\] angee: running command argv="git fetch"\n` +
			`\[\+\d+\.\d{3}s\] warning: slow command\n$`
		if !regexp.MustCompile(pattern).MatchString(output.String()) {
			t.Fatalf("output = %q, want elapsed debug prefixes", output.String())
		}
	})

	t.Run("structured attrs", func(t *testing.T) {
		var output bytes.Buffer
		handler := NewCLIHandler(&output, slog.LevelInfo).
			WithAttrs([]slog.Attr{slog.String("component", "operator api")}).
			WithGroup("request")
		logger := slog.New(handler)
		logger.Info("handled", slog.Int("status", 200), slog.Group("timing", slog.Duration("duration", time.Second)))

		want := "angee: handled component=\"operator api\" request.status=200 request.timing.duration=1s\n"
		if got := output.String(); got != want {
			t.Fatalf("output = %q, want %q", got, want)
		}
	})
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "username and password", in: "https://alexis:secret@example.com/repo?q=1", want: "https://%2A%2A%2A@example.com/repo?q=1"},
		{name: "username", in: "ssh://git@example.com/repo", want: "ssh://%2A%2A%2A@example.com/repo"},
		{name: "no userinfo", in: "https://example.com/repo", want: "https://example.com/repo"},
		{name: "invalid", in: "://%", want: "://%"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RedactURL(test.in); got != test.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestRedactArgs(t *testing.T) {
	args := []string{
		"deploy",
		"--token", "secret-token",
		"--password=hunter2",
		"--jwt-secret", "signed",
		"ssh://git:credential@example.com/repo",
		"--other=value",
	}
	want := []string{
		"deploy",
		"--token", "***",
		"--password=***",
		"--jwt-secret", "***",
		"ssh://%2A%2A%2A@example.com/repo",
		"--other=value",
	}
	got := RedactArgs(args)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("RedactArgs() = %#v, want %#v", got, want)
	}
	if args[2] != "secret-token" || args[3] != "--password=hunter2" {
		t.Fatalf("RedactArgs mutated input: %#v", args)
	}
}

func TestEnvKeys(t *testing.T) {
	got := EnvKeys([]string{"PATH=/bin", "TOKEN=secret=value", "EMPTY=", "INVALID", "=missing"})
	want := []string{"PATH", "TOKEN", "EMPTY"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("EnvKeys() = %#v, want %#v", got, want)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		max  int
		want string
	}{
		{name: "under limit", in: []byte("hello"), max: 5, want: "hello"},
		{name: "truncated", in: []byte("abcdefgh"), max: 5, want: "abcde … (3 bytes truncated)"},
		{name: "zero", in: []byte("abc"), max: 0, want: " … (3 bytes truncated)"},
		{name: "negative", in: []byte("abc"), max: -1, want: " … (3 bytes truncated)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Truncate(test.in, test.max); got != test.want {
				t.Fatalf("Truncate() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFromWithoutLogger(t *testing.T) {
	logger := From(context.Background())
	if logger == nil {
		t.Fatal("From returned nil")
	}
	if logger.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("default logger unexpectedly enabled")
	}

	ctx := With(context.Background(), nil)
	if got := From(ctx); got == nil || got.Enabled(ctx, slog.LevelError) {
		t.Fatal("With(nil) did not install a discard logger")
	}
}

func TestStepInfoHeartbeatAndCompletion(t *testing.T) {
	withStepTimings(t, 10*time.Millisecond, 15*time.Millisecond, 40*time.Millisecond)
	synctest.Test(t, func(t *testing.T) {
		var output synchronizedBuffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelDebug))
		ctx := With(t.Context(), logger)
		complete, stopped := startStep(ctx, "refreshing source", slog.String("source", "django"))

		synctest.Wait()
		if got := output.String(); !strings.Contains(got, "angee: refreshing source source=django") {
			t.Fatalf("start output = %q", got)
		}

		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if got := output.String(); !strings.Contains(got, "angee: still refreshing source (10ms) source=django") {
			t.Fatalf("first heartbeat output = %q", got)
		}

		time.Sleep(15 * time.Millisecond)
		synctest.Wait()
		if got := output.String(); !strings.Contains(got, "angee: still refreshing source (25ms) source=django") {
			t.Fatalf("second heartbeat output = %q", got)
		}

		complete(nil)
		select {
		case <-stopped:
		default:
			t.Fatal("heartbeat goroutine did not stop before completion returned")
		}
		if got := output.String(); !strings.Contains(got, "angee: finished refreshing source source=django duration=25ms") {
			t.Fatalf("completion output = %q", got)
		}

		before := output.String()
		complete(errors.New("ignored"))
		if got := output.String(); got != before {
			t.Fatalf("second completion logged output: before %q, after %q", before, got)
		}
	})
}

func TestStepWarnHeartbeatAndFailure(t *testing.T) {
	withStepTimings(t, 10*time.Millisecond, 15*time.Millisecond, 30*time.Millisecond)
	synctest.Test(t, func(t *testing.T) {
		var output synchronizedBuffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelWarn))
		ctx := With(t.Context(), logger)
		complete, stopped := startStep(ctx, "waiting for lock")

		time.Sleep(30 * time.Millisecond)
		synctest.Wait()
		if got, want := output.String(), "warning: still waiting for lock after 30ms\n"; got != want {
			t.Fatalf("heartbeat output = %q, want %q", got, want)
		}

		complete(errors.New("lock unavailable"))
		select {
		case <-stopped:
		default:
			t.Fatal("heartbeat goroutine did not stop before completion returned")
		}
		if got := output.String(); !strings.Contains(got, "warning: finished waiting for lock duration=30ms err=\"lock unavailable\"\n") {
			t.Fatalf("failure output = %q", got)
		}
	})
}

func TestStepContextCancellationStopsHeartbeat(t *testing.T) {
	withStepTimings(t, 10*time.Millisecond, 15*time.Millisecond, 30*time.Millisecond)
	synctest.Test(t, func(t *testing.T) {
		var output synchronizedBuffer
		logger := slog.New(NewCLIHandler(&output, slog.LevelInfo))
		base, cancel := context.WithCancel(t.Context())
		_, stopped := startStep(With(base, logger), "canceled step")
		cancel()
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Fatal("heartbeat goroutine leaked after context cancellation")
		}
	})
}

func withStepTimings(t *testing.T, firstInfo, infoInterval, warn time.Duration) {
	t.Helper()
	oldFirstInfo := stepFirstInfoDelay
	oldInfoInterval := stepInfoInterval
	oldWarn := stepWarnDelay
	stepFirstInfoDelay = firstInfo
	stepInfoInterval = infoInterval
	stepWarnDelay = warn
	t.Cleanup(func() {
		stepFirstInfoDelay = oldFirstInfo
		stepInfoInterval = oldInfoInterval
		stepWarnDelay = oldWarn
	})
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
