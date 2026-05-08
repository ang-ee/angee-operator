package proccompose

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fyltr/angee/internal/runtime"
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
	out, err := cmd.CombinedOutput()
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
	args := b.baseArgs(target.Root)
	// `-d` daemonises; `--tui=false` prevents the supervisor from trying
	// to attach a TUI on a process that has no controlling terminal
	// (which is the normal case for `angee workspace start` running
	// from a non-interactive shell or under another supervisor).
	args = append(args, "up", "-d", "--tui=false")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) UpForeground(ctx context.Context, target runtime.Target, stdout io.Writer, stderr io.Writer) error {
	args := b.baseArgs(target.Root)
	args = append(args, "up", "--tui=false")
	args = append(args, target.Services...)
	return b.runForeground(ctx, target.Root, target.EnvFile, stdout, stderr, args...)
}

func (b Backend) Down(ctx context.Context, root string) error {
	args := b.baseArgs(root)
	args = append(args, "down")
	_, err := b.run(ctx, root, "", args...)
	return err
}

func (b Backend) Start(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root)
	args = append(args, "process", "start")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Stop(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root)
	args = append(args, "process", "stop")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Restart(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root)
	args = append(args, "process", "restart")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, target.EnvFile, args...)
	return err
}

func (b Backend) Logs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.baseArgs(req.Root)
	args = append(args, "logs")
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

func (b Backend) Status(context.Context, string) ([]runtime.ServiceStatus, error) {
	return nil, nil
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
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(buf.Bytes())))
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
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
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
	out, err := exec.CommandContext(ctx, "go", "env", "GOPATH").Output()
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
		cmd := exec.CommandContext(ctx, "go", "install", processComposeInstallPackage)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		return cmd.Run()
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

func (b Backend) baseArgs(root string) []string {
	return []string{"-f", filepath.Join(root, "process-compose.yaml")}
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
