package proccompose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

const processComposeInstallPackage = "github.com/f1bonacc1/process-compose@latest"

type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	trace := logctx.TraceExec(ctx, name, args, dir, slog.Any("env", logctx.EnvKeys(env)))
	out, err := cmd.CombinedOutput()
	trace(out, err)
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type Backend struct {
	Runner                Runner
	Stdin                 io.Reader
	LookupPath            func(string) (string, error)
	GoBinPath             func(context.Context) (string, error)
	InstallProcessCompose func(context.Context, io.Writer, io.Writer) error
}

func NewBackend() Backend {
	return Backend{Runner: ExecRunner{}}
}

func (b Backend) Build(context.Context, runtime.Target) error {
	return nil
}

func (b Backend) Up(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.ControlPort)
	// `-d` daemonises; `--tui=false` prevents the supervisor from trying
	// to attach a TUI on a process that has no controlling terminal
	// (which is the normal case for `angee stack up --root ...` against
	// a workspace's inner stack from a non-interactive shell or under
	// another supervisor).
	args = append(args, "up", "-d", "--tui=false")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) UpForeground(ctx context.Context, target runtime.Target, stdout io.Writer, stderr io.Writer) error {
	args := b.baseArgs(target.Root, target.ControlPort)
	args = append(args, "up", "--tui=false")
	args = append(args, target.Services...)
	return b.runForeground(ctx, target.Root, target.EnvFile, stdout, stderr, args...)
}

func (b Backend) Down(ctx context.Context, target runtime.Target) error {
	// `down` is a CLIENT command in process-compose v2 — it connects to
	// the running supervisor and asks it to terminate. Do NOT pass -f
	// (config-file flag is for `up`, the server command); doing so makes
	// process-compose print --help and exit 0.
	args := b.clientArgs(target.ControlPort)
	args = append(args, "down")
	_, err := b.run(ctx, target.Root, "", args...)
	return err
}

func (b Backend) Start(ctx context.Context, target runtime.Target) error {
	args := b.clientArgs(target.ControlPort)
	args = append(args, "process", "start")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Stop(ctx context.Context, target runtime.Target) error {
	args := b.clientArgs(target.ControlPort)
	args = append(args, "process", "stop")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Restart(ctx context.Context, target runtime.Target) error {
	args := b.clientArgs(target.ControlPort)
	args = append(args, "process", "restart")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Logs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.clientArgs(req.ControlPort)
	args = append(args, "process", "logs")
	if req.Follow {
		args = append(args, "--follow")
	}
	args = append(args, req.Services...)
	var (
		out []byte
		err error
	)
	if req.MaxBytes > 0 {
		out, err = b.runLimited(ctx, req.Root, req.EnvFile, req.MaxBytes, args...)
	} else {
		out, err = b.run(ctx, req.Root, req.EnvFile, args...)
	}
	if err != nil {
		return nil, err
	}
	ch := make(chan string, 1)
	ch <- string(out)
	close(ch)
	return ch, nil
}

// StreamLogs runs `process-compose process logs [--follow]` and streams its
// combined output one line per channel element, closing on process exit or ctx
// cancel. Unlike Logs it never buffers, so a `--follow` stream surfaces lines
// live.
func (b Backend) StreamLogs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.clientArgs(req.ControlPort)
	args = append(args, "process", "logs")
	if req.Follow {
		args = append(args, "--follow")
	}
	if req.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(req.Tail))
	}
	args = append(args, req.Services...)
	// A test Runner can't stream a live process, so capture through it and
	// replay by line. The real ExecRunner streams below.
	if b.Runner != nil && !isExecRunner(b.Runner) {
		out, err := b.run(ctx, req.Root, req.EnvFile, args...)
		if err != nil {
			return nil, err
		}
		return runtime.ReplayLines(ctx, out), nil
	}
	name, err := b.processComposeBinary(ctx, nil, nil, nil, false)
	if err != nil {
		return nil, err
	}
	env, err := readEnvFile(req.EnvFile)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = req.Root
	cmd.Env = append(os.Environ(), env...)
	ch, err := runtime.StreamCommand(ctx, cmd, slog.Any("env", logctx.EnvKeys(env)))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return ch, nil
}

func (b Backend) Status(ctx context.Context, req runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	args := b.clientArgs(req.ControlPort)
	args = append(args, "list", "-o", "json")
	out, err := b.run(ctx, req.Root, "", args...)
	if err != nil {
		return nil, err
	}
	return parseList(out)
}

type processListEntry struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	IsRunning bool   `json:"is_running"`
	ExitCode  int    `json:"exit_code"`
	IsReady   string `json:"is_ready"`
}

func parseList(data []byte) ([]runtime.ServiceStatus, error) {
	entries, err := decodeProcessList(data)
	if err != nil {
		return nil, err
	}
	statuses := make([]runtime.ServiceStatus, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == "" {
			continue
		}
		statuses = append(statuses, runtime.ServiceStatus{
			Name:    entry.Name,
			Runtime: "local",
			State:   strings.ToLower(strings.TrimSpace(entry.Status)),
			Health:  procHealth(entry),
		})
	}
	return statuses, nil
}

// decodeProcessList extracts the JSON array that `process-compose list -o json`
// prints. process-compose merges human-readable banner lines ahead of the array
// (ExecRunner folds stderr in via CombinedOutput) — including ANSI-coloured
// notices such as "New version available" whose escape codes (e.g. "\x1b[33m")
// contain a '['. So the array does not necessarily begin at the first '[': try
// each '[' as a candidate start and decode the first JSON value from there,
// which also tolerates any trailing banner text after the array.
func decodeProcessList(data []byte) ([]processListEntry, error) {
	trimmed := bytes.TrimSpace(data)
	var lastErr error
	for {
		idx := bytes.IndexByte(trimmed, '[')
		if idx < 0 {
			if lastErr != nil {
				return nil, fmt.Errorf("parse process-compose status: %w", lastErr)
			}
			return nil, errors.New("parse process-compose status: JSON array not found")
		}
		var entries []processListEntry
		if err := json.NewDecoder(bytes.NewReader(trimmed[idx:])).Decode(&entries); err == nil {
			return entries, nil
		} else {
			lastErr = err
		}
		trimmed = trimmed[idx+1:]
	}
}

func procHealth(entry processListEntry) string {
	// process-compose surfaces readiness through `is_ready`. When a
	// service has no ready probe declared, the value is "-" and we
	// leave Health empty (matching docker's "no healthcheck" case).
	switch strings.ToLower(strings.TrimSpace(entry.IsReady)) {
	case "ready":
		return "healthy"
	case "not ready", "notready":
		return "unhealthy"
	}
	return ""
}

func (b Backend) run(ctx context.Context, root string, envFile string, args ...string) ([]byte, error) {
	if b.Runner == nil {
		b.Runner = ExecRunner{}
	}
	name := "process-compose"
	if isExecRunner(b.Runner) {
		var err error
		name, err = b.processComposeBinary(ctx, nil, nil, nil, false)
		if err != nil {
			return nil, err
		}
	}
	env, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	return b.Runner.Run(ctx, root, env, name, args...)
}

func (b Backend) runLimited(ctx context.Context, root string, envFile string, maxBytes int, args ...string) ([]byte, error) {
	if b.Runner != nil {
		if !isExecRunner(b.Runner) {
			return b.run(ctx, root, envFile, args...)
		}
	}
	name, err := b.processComposeBinary(ctx, nil, nil, nil, false)
	if err != nil {
		return nil, err
	}
	env, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	buf := &limitedBuffer{remaining: maxBytes}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = buf
	cmd.Stderr = buf
	trace := logctx.TraceExec(ctx, name, args, root, slog.Any("env", logctx.EnvKeys(env)))
	runErr := cmd.Run()
	trace(buf.Bytes(), runErr)
	if runErr != nil {
		return buf.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), runErr, strings.TrimSpace(string(buf.Bytes())))
	}
	return buf.Bytes(), nil
}

func (b Backend) runForeground(ctx context.Context, root string, envFile string, stdout io.Writer, stderr io.Writer, args ...string) error {
	name, err := b.processComposeBinary(ctx, b.input(), stdout, stderr, true)
	if err != nil {
		return err
	}
	env, err := readEnvFile(envFile)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = runtime.GracefulWaitDelay
	trace := logctx.TraceExec(ctx, name, args, root, slog.Any("env", logctx.EnvKeys(env)))
	runErr := cmd.Run()
	trace(nil, runErr)
	if runErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), runErr)
	}
	return nil
}

func (b Backend) processComposeBinary(ctx context.Context, stdin io.Reader, stdout io.Writer, stderr io.Writer, prompt bool) (string, error) {
	if path, err := b.lookupPath()("process-compose"); err == nil {
		return path, nil
	}
	if path, err := b.goBinProcessCompose(ctx); err == nil {
		return path, nil
	}
	if !prompt || !canPrompt(stdin, b.Stdin != nil) {
		return "", missingProcessComposeError()
	}
	if !confirmInstall(stdin, stderr) {
		return "", missingProcessComposeError()
	}
	if err := b.installProcessCompose()(ctx, stdout, stderr); err != nil {
		return "", fmt.Errorf("install process-compose: %w", err)
	}
	if path, err := b.lookupPath()("process-compose"); err == nil {
		return path, nil
	}
	if path, err := b.goBinProcessCompose(ctx); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("process-compose was installed but is not executable; add $(go env GOPATH)/bin to PATH")
}

func (b Backend) lookupPath() func(string) (string, error) {
	if b.LookupPath != nil {
		return b.LookupPath
	}
	return exec.LookPath
}

func (b Backend) input() io.Reader {
	if b.Stdin != nil {
		return b.Stdin
	}
	return os.Stdin
}

func (b Backend) goBinProcessCompose(ctx context.Context) (string, error) {
	goBin, err := b.goBinPath(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(goBin, "process-compose")
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

func (b Backend) goBinPath(ctx context.Context) (string, error) {
	if b.GoBinPath != nil {
		return b.GoBinPath(ctx)
	}
	args := []string{"env", "GOPATH"}
	trace := logctx.TraceExec(ctx, "go", args, "")
	out, err := exec.CommandContext(ctx, "go", args...).Output()
	trace(out, err)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("GOPATH is empty")
	}
	return filepath.Join(path, "bin"), nil
}

func (b Backend) installProcessCompose() func(context.Context, io.Writer, io.Writer) error {
	if b.InstallProcessCompose != nil {
		return b.InstallProcessCompose
	}
	return func(ctx context.Context, stdout io.Writer, stderr io.Writer) error {
		args := []string{"install", processComposeInstallPackage}
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		trace := logctx.TraceExec(ctx, "go", args, "")
		err := cmd.Run()
		trace(nil, err)
		return err
	}
}

func canPrompt(stdin io.Reader, explicit bool) bool {
	if stdin == nil {
		return false
	}
	if explicit {
		return true
	}
	f, ok := stdin.(*os.File)
	if !ok {
		return true
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

func confirmInstall(stdin io.Reader, stderr io.Writer) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	_, _ = fmt.Fprintf(stderr, "process-compose is required but was not found. Install it now with `go install %s`? [y/N] ", processComposeInstallPackage)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		_, _ = fmt.Fprintln(stderr)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func missingProcessComposeError() error {
	return fmt.Errorf("process-compose is required; install it with `go install %s` or add it to PATH", processComposeInstallPackage)
}

func isExecRunner(r Runner) bool {
	switch r.(type) {
	case ExecRunner, *ExecRunner:
		return true
	default:
		return false
	}
}

type limitedBuffer struct {
	data      []byte
	remaining int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	accepted := len(p)
	if b.remaining <= 0 {
		b.truncated = true
		return accepted, nil
	}
	if len(p) > b.remaining {
		b.data = append(b.data, p[:b.remaining]...)
		b.remaining = 0
		b.truncated = true
		return accepted, nil
	}
	b.data = append(b.data, p...)
	b.remaining -= len(p)
	return accepted, nil
}

func (b *limitedBuffer) Bytes() []byte {
	if !b.truncated {
		return b.data
	}
	out := append([]byte{}, b.data...)
	out = append(out, []byte("\n[truncated]\n")...)
	return out
}

func (b Backend) baseArgs(root string, controlPort int) []string {
	args := []string{"-f", filepath.Join(root, "process-compose.yaml")}
	return append(args, b.clientArgs(controlPort)...)
}

func (b Backend) clientArgs(controlPort int) []string {
	if controlPort <= 0 {
		controlPort = 8080
	}
	return []string{"--address", "127.0.0.1", "--port", strconv.Itoa(controlPort)}
}

func readEnvFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var env []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		env = append(env, strings.TrimSpace(key)+"="+value)
	}
	return env, scanner.Err()
}
