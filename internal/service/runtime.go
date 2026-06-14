package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"golang.org/x/sync/errgroup"
)

const defaultProcessComposeControlPort = 8080

func (p *Platform) StackBuild(ctx context.Context, services []string) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if err := p.bootstrapOpenBao(ctx, stack, nil, nil); err != nil {
		return err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return err
	}
	selected, err := selectRuntimeServices(stack, services, manifest.RuntimeContainer)
	if err != nil {
		return err
	}
	if len(compiled.Compose.Services) == 0 || len(selected) == 0 && len(services) > 0 {
		return nil
	}
	return p.composeBackend.Build(ctx, runtime.Target{Root: p.root, Services: selected, EnvFile: p.runtimeEnvFile(stack)})
}

func (p *Platform) StackUp(ctx context.Context, services []string, build bool) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if err := p.bootstrapOpenBao(ctx, stack, nil, nil); err != nil {
		return err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return err
	}
	selected, err := selectRuntimeServices(stack, services, manifest.RuntimeContainer)
	if err != nil {
		return err
	}
	if len(compiled.Compose.Services) == 0 || len(selected) == 0 && len(services) > 0 {
		return nil
	}
	return p.composeBackend.Up(ctx, runtime.Target{Root: p.root, Services: selected, Build: build, EnvFile: p.runtimeEnvFile(stack)})
}

func (p *Platform) StackUpForeground(ctx context.Context, services []string, build bool, stdout io.Writer, stderr io.Writer) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if err := p.bootstrapOpenBao(ctx, stack, stdout, stderr); err != nil {
		return err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return err
	}
	selected, err := selectRuntimeServices(stack, services, manifest.RuntimeContainer)
	if err != nil {
		return err
	}
	if len(compiled.Compose.Services) == 0 || len(selected) == 0 && len(services) > 0 {
		return nil
	}
	return p.composeBackend.UpForeground(ctx, runtime.Target{Root: p.root, Services: selected, Build: build, EnvFile: p.runtimeEnvFile(stack)}, stdout, stderr)
}

func (p *Platform) StackDev(ctx context.Context, build bool) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if err := p.bootstrapOpenBao(ctx, stack, nil, nil); err != nil {
		return err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return err
	}
	if len(compiled.Compose.Services) > 0 {
		if err := p.composeBackend.Up(ctx, runtime.Target{Root: p.root, Build: build, EnvFile: p.runtimeEnvFile(stack)}); err != nil {
			return err
		}
	}
	if len(compiled.ProcessCompose.Processes) > 0 {
		if err := p.procBackend.Up(ctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack)}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) StackDevForeground(ctx context.Context, build bool, stdout io.Writer, stderr io.Writer) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if err := p.bootstrapOpenBao(ctx, stack, stdout, stderr); err != nil {
		return err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return err
	}
	hasContainers := len(compiled.Compose.Services) > 0
	hasLocal := len(compiled.ProcessCompose.Processes) > 0

	// Nothing rendered to run yet: fall back to following whatever logs the
	// backends can produce so `angee dev` against an empty stack still tails.
	if !hasContainers && !hasLocal {
		logs, err := p.StackLogs(ctx, nil, true)
		if err != nil {
			return err
		}
		for line := range logs {
			if _, err := io.WriteString(stdout, line); err != nil {
				return err
			}
		}
		return nil
	}

	// Build container images up front when requested, before any service
	// starts. Building inside the concurrent group would race a local service
	// that depends on a freshly-built container against the build itself.
	if build && hasContainers {
		if err := p.composeBackend.Build(ctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack)}); err != nil {
			return err
		}
	}

	// stdout/stderr may be a single shared writer (the operator streams both
	// through one HTTP response). os/exec hands a child the terminal directly
	// only when the sink is an *os.File; for anything else it runs a copier
	// goroutine, so the two backends would race on the shared writer. Guard
	// non-file sinks with a mutex, but pass *os.File sinks through untouched so
	// the children keep the real TTY — and with it docker compose's and
	// process-compose's per-service colouring.
	so, se := stdout, stderr
	if stdout == stderr {
		if w := guardDevSink(stdout); w != stdout {
			so, se = w, w
		}
	} else {
		so = guardDevSink(stdout)
		se = guardDevSink(stderr)
	}

	// Run both runtimes attached in the foreground so logs from every service
	// stream together — docker compose keeps its native per-service coloured
	// prefix, process-compose streams its own aggregated output. Each call
	// blocks until interrupted, so they run concurrently. The derived cancel
	// makes the first backend to exit — cleanly or not — tear the other down:
	// errgroup's own context only cancels on a non-nil error, and an attached
	// compose run returns nil on graceful shutdown, so we can't rely on it.
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gctx := errgroup.WithContext(groupCtx)
	if hasContainers {
		g.Go(func() error {
			defer cancel()
			return p.composeBackend.UpForeground(gctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack), Attached: true}, so, se)
		})
	}
	if hasLocal {
		g.Go(func() error {
			defer cancel()
			return p.procBackend.UpForeground(gctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack)}, so, se)
		})
	}
	return g.Wait()
}

// guardDevSink wraps w so concurrent writes from the two dev backends serialize,
// unless w is an *os.File: exec passes those to each child as the real terminal
// fd (no parent-side copier goroutine, so no Go-level race), and the children
// need that TTY to keep their colouring.
func guardDevSink(w io.Writer) io.Writer {
	if _, ok := w.(*os.File); ok {
		return w
	}
	return &syncWriter{w: w}
}

// syncWriter serializes concurrent writes from the two dev backends to a
// shared, non-*os.File sink (e.g. the operator's HTTP response writer). os/exec
// runs a copier goroutine per child process for such sinks, so without this the
// two streams race on the underlying writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (p *Platform) StackDown(ctx context.Context) error {
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	hasContainers := false
	hasLocal := false
	for _, service := range stack.Services {
		switch service.Runtime {
		case manifest.RuntimeContainer:
			hasContainers = true
		case manifest.RuntimeLocal:
			hasLocal = true
		}
	}
	for _, job := range stack.Jobs {
		if job.Runtime == manifest.RuntimeLocal {
			hasLocal = true
		}
	}
	if hasContainers {
		if err := p.composeBackend.Down(ctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack)}); err != nil {
			return err
		}
	}
	if hasLocal {
		return p.procBackend.Down(ctx, runtime.Target{Root: p.root, ControlPort: processComposeControlPort(stack)})
	}
	return nil
}

func (p *Platform) ServiceUp(ctx context.Context, names []string) error {
	return p.serviceRuntimeAction(ctx, "up", names)
}

func (p *Platform) ServiceStart(ctx context.Context, names []string) error {
	return p.serviceRuntimeAction(ctx, "start", names)
}

func (p *Platform) ServiceStop(ctx context.Context, names []string) error {
	return p.serviceRuntimeAction(ctx, "stop", names)
}

func (p *Platform) ServiceRestart(ctx context.Context, names []string) error {
	return p.serviceRuntimeAction(ctx, "restart", names)
}

func (p *Platform) StackLogs(ctx context.Context, services []string, follow bool) (<-chan string, error) {
	return p.StackLogsLimited(ctx, services, follow, 0)
}

func (p *Platform) StackLogsLimited(ctx context.Context, services []string, follow bool, maxBytes int) (<-chan string, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	compiled, err := p.StackPrepare(ctx)
	if err != nil {
		return nil, err
	}
	container := []string{}
	local := []string{}
	if len(services) == 0 {
		for _, name := range sortedKeys(stack.Services) {
			switch stack.Services[name].Runtime {
			case manifest.RuntimeContainer:
				container = append(container, name)
			case manifest.RuntimeLocal:
				local = append(local, name)
			}
		}
	} else {
		container, local, err = splitRuntimeServices(stack, services)
		if err != nil {
			return nil, err
		}
	}
	var channels []<-chan string
	if len(compiled.Compose.Services) > 0 && len(container) > 0 {
		ch, err := p.composeBackend.Logs(ctx, runtime.LogsRequest{Root: p.root, Services: container, Follow: follow, EnvFile: p.runtimeEnvFile(stack), MaxBytes: maxBytes})
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	if len(compiled.ProcessCompose.Processes) > 0 && len(local) > 0 {
		ch, err := p.procBackend.Logs(ctx, runtime.LogsRequest{Root: p.root, Services: local, Follow: follow, EnvFile: p.runtimeEnvFile(stack), MaxBytes: maxBytes, ControlPort: processComposeControlPort(stack)})
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	if len(channels) == 0 {
		ch := make(chan string)
		close(ch)
		return ch, nil
	}
	out := make(chan string)
	go func() {
		defer close(out)
		for _, ch := range channels {
			for line := range ch {
				out <- line
			}
		}
	}()
	return out, nil
}

func (p *Platform) serviceRuntimeAction(ctx context.Context, action string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one service name is required")
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	if action == "up" {
		if err := p.bootstrapOpenBao(ctx, stack, nil, nil); err != nil {
			return err
		}
	}
	if _, err := p.StackPrepare(ctx); err != nil {
		return err
	}
	container, local, err := splitRuntimeServices(stack, names)
	if err != nil {
		return err
	}
	containerTarget := runtime.Target{Root: p.root, Services: container, EnvFile: p.runtimeEnvFile(stack)}
	localTarget := runtime.Target{Root: p.root, Services: local, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack)}
	switch action {
	case "up":
		if len(container) > 0 {
			if err := p.composeBackend.Up(ctx, containerTarget); err != nil {
				return err
			}
		}
		if len(local) > 0 {
			return p.procBackend.Up(ctx, localTarget)
		}
		return nil
	case "start":
		if len(container) > 0 {
			if err := p.composeBackend.Start(ctx, containerTarget); err != nil {
				return err
			}
		}
		if len(local) > 0 {
			return p.procBackend.Start(ctx, localTarget)
		}
		return nil
	case "stop":
		if len(container) > 0 {
			if err := p.composeBackend.Stop(ctx, containerTarget); err != nil {
				return err
			}
		}
		if len(local) > 0 {
			return p.procBackend.Stop(ctx, localTarget)
		}
		return nil
	case "restart":
		if len(container) > 0 {
			if err := p.composeBackend.Restart(ctx, containerTarget); err != nil {
				return err
			}
		}
		if len(local) > 0 {
			return p.procBackend.Restart(ctx, localTarget)
		}
		return nil
	default:
		return fmt.Errorf("unknown service runtime action %q", action)
	}
}

func processComposeControlPort(stack *manifest.Stack) int {
	if stack == nil {
		return defaultProcessComposeControlPort
	}
	if port, ok := stack.Ports["process_compose"]; ok && port.Value > 0 {
		return port.Value
	}
	if port, ok := stack.Ports["process-compose"]; ok && port.Value > 0 {
		return port.Value
	}
	return defaultProcessComposeControlPort
}

func splitRuntimeServices(stack *manifest.Stack, names []string) ([]string, []string, error) {
	container := []string{}
	local := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		service, ok := stack.Services[name]
		if !ok {
			return nil, nil, &NotFoundError{Kind: "service", Name: name}
		}
		switch service.Runtime {
		case manifest.RuntimeContainer:
			container = append(container, name)
		case manifest.RuntimeLocal:
			local = append(local, name)
		default:
			return nil, nil, fmt.Errorf("service %q has unsupported runtime %q", name, service.Runtime)
		}
	}
	return container, local, nil
}

func selectRuntimeServices(stack *manifest.Stack, names []string, runtimeKind manifest.Runtime) ([]string, error) {
	if len(names) == 0 {
		selected := make([]string, 0)
		for _, name := range sortedKeys(stack.Services) {
			if stack.Services[name].Runtime == runtimeKind {
				selected = append(selected, name)
			}
		}
		return selected, nil
	}
	selected := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		service, ok := stack.Services[name]
		if !ok {
			return nil, &NotFoundError{Kind: "service", Name: name}
		}
		if service.Runtime != runtimeKind {
			return nil, fmt.Errorf("service %q uses runtime %q, not %q", name, service.Runtime, runtimeKind)
		}
		selected = append(selected, name)
	}
	return selected, nil
}
