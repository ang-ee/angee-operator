package compose

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fyltr/angee/internal/runtime"
)

type Runner interface {
	Run(ctx context.Context, dir string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type Backend struct {
	Runner Runner
}

func NewBackend() Backend {
	return Backend{Runner: ExecRunner{}}
}

func (b Backend) Build(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "build")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) Up(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "up", "-d")
	if target.Build {
		args = append(args, "--build")
	}
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) UpForeground(ctx context.Context, target runtime.Target, stdout io.Writer, stderr io.Writer) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "up", "-d")
	if target.Build {
		args = append(args, "--build")
	}
	args = append(args, target.Services...)
	return b.runForeground(ctx, target.Root, stdout, stderr, args...)
}

func (b Backend) Down(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "down")
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) Start(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "start")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) Stop(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "stop")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) Restart(ctx context.Context, target runtime.Target) error {
	args := b.baseArgs(target.Root, target.EnvFile)
	args = append(args, "restart")
	args = append(args, target.Services...)
	_, err := b.run(ctx, target.Root, args...)
	return err
}

func (b Backend) Logs(ctx context.Context, req runtime.LogsRequest) (<-chan string, error) {
	args := b.baseArgs(req.Root, req.EnvFile)
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
		out, err = b.runLimited(ctx, req.Root, req.MaxBytes, args...)
	} else {
		out, err = b.run(ctx, req.Root, args...)
	}
	if err != nil {
		return nil, err
	}
	ch := make(chan string, 1)
	ch <- string(out)
	close(ch)
	return ch, nil
}

func (b Backend) Status(ctx context.Context, req runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	args := b.baseArgs(req.Root, "")
	args = append(args, "ps", "--format", "json")
	out, err := b.run(ctx, req.Root, args...)
	if err != nil {
		return nil, err
	}
	return parsePS(out), nil
}

func (b Backend) run(ctx context.Context, root string, args ...string) ([]byte, error) {
	if b.Runner == nil {
		b.Runner = ExecRunner{}
	}
	return b.Runner.Run(ctx, root, "docker", args...)
}

func (b Backend) runLimited(ctx context.Context, root string, maxBytes int, args ...string) ([]byte, error) {
	if b.Runner != nil {
		if !isExecRunner(b.Runner) {
			return b.run(ctx, root, args...)
		}
	}
	buf := &limitedBuffer{remaining: maxBytes}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = root
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(buf.Bytes())))
	}
	return buf.Bytes(), nil
}

func (b Backend) runForeground(ctx context.Context, root string, stdout io.Writer, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (b Backend) baseArgs(root, envFile string) []string {
	args := []string{"compose", "-f", filepath.Join(root, "docker-compose.yaml")}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	return args
}

func parsePS(data []byte) []runtime.ServiceStatus {
	var statuses []runtime.ServiceStatus
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var one struct {
			Service string `json:"Service"`
			Name    string `json:"Name"`
			State   string `json:"State"`
			Health  string `json:"Health"`
		}
		if err := json.Unmarshal([]byte(line), &one); err != nil {
			continue
		}
		name := one.Service
		if name == "" {
			name = one.Name
		}
		if name == "" {
			continue
		}
		statuses = append(statuses, runtime.ServiceStatus{Name: name, Runtime: "container", State: one.State, Health: one.Health})
	}
	return statuses
}

var ErrNoServices = errors.New("no container services selected")

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
