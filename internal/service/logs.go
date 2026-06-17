package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

// StreamServiceLogs opens a live, line-by-line log stream for a single service
// and wraps each line into a structured LogLine. Because the call is scoped to
// one named service, attribution is exact — the service name comes from the
// argument, not from parsing a prefix — and the backend is chosen by the
// service's runtime. The channel closes when the underlying stream ends or ctx
// is done; cancelling ctx tears down the upstream follow process.
//
// tail, when > 0, replays the last N lines before the live follow begins
// (docker compose / process-compose `--tail`); 0 defers to the backend's
// default backlog.
func (p *Platform) StreamServiceLogs(ctx context.Context, service string, tail int) (<-chan api.LogLine, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	svc, ok := stack.Services[service]
	if !ok {
		return nil, fmt.Errorf("service %q not found", service)
	}
	if _, err := p.StackPrepare(ctx); err != nil {
		return nil, err
	}

	req := runtime.LogsRequest{
		Root:     p.root,
		Services: []string{service},
		Follow:   true,
		NoPrefix: true,
		Tail:     tail,
		EnvFile:  p.runtimeEnvFile(stack),
	}
	var lines <-chan string
	switch svc.Runtime {
	case manifest.RuntimeContainer:
		lines, err = p.composeBackend.StreamLogs(ctx, req)
	case manifest.RuntimeLocal:
		req.ControlPort = processComposeControlPort(stack)
		lines, err = p.procBackend.StreamLogs(ctx, req)
	default:
		return nil, fmt.Errorf("service %q has unsupported runtime %q", service, svc.Runtime)
	}
	if err != nil {
		return nil, fmt.Errorf("streaming logs for service %q: %w", service, err)
	}

	runtimeKind := string(svc.Runtime)
	out := make(chan api.LogLine)
	go func() {
		defer close(out)
		for line := range lines {
			frame := api.LogLine{
				Service: service,
				Runtime: runtimeKind,
				Message: strings.TrimRight(line, "\r\n"),
				Level:   inferLogLevel(line),
			}
			select {
			case out <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// logLevelPattern matches common severity markers — `level=warn`, `[ERROR]`, a
// leading `INFO:` — without matching the words mid-sentence. Severity is NOT
// supplied by docker compose or process-compose per line, so this is
// deliberately best-effort: it returns nil when nothing matches.
var logLevelPattern = regexp.MustCompile(`(?i)(?:\blevel\s*["']?\s*[=:]\s*["']?|(?:^|\s)\[)(trace|debug|info|warn(?:ing)?|error|err|fatal|panic)\b`)

func inferLogLevel(line string) *string {
	m := logLevelPattern.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	level := strings.ToUpper(m[1])
	switch level {
	case "WARNING":
		level = "WARN"
	case "ERR":
		level = "ERROR"
	case "FATAL", "PANIC":
		level = "ERROR"
	}
	return &level
}
