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

// logTail resolves the tail a log read or follow asks the runtimes for: 0 means
// runtime.DefaultLogTail, and a tail above runtime.MaxLogTail is capped at it.
// A follow of a local (process-compose) service is refused a backlog above
// runtime.MaxLocalFollowTail, because process-compose drops the newest lines of
// a longer replay; its whole output is in run/logs/<name>.log. A buffered read
// of a local service is bounded only by the supervisor's buffer.
func logTail(tail int, local []string, follow bool) (int, error) {
	switch {
	case tail < 0:
		return 0, &InvalidInputError{Field: "tail", Reason: "must not be negative"}
	case tail == 0:
		return runtime.DefaultLogTail, nil
	case follow && tail > runtime.MaxLocalFollowTail && len(local) > 0:
		files := make([]string, 0, len(local))
		for _, name := range local {
			files = append(files, "run/logs/"+name+".log")
		}
		return 0, &InvalidInputError{Field: "tail", Reason: fmt.Sprintf(
			"%d is above %d, the most lines process-compose replays whole when following a local service; read without following, or read %s under the stack root",
			tail, runtime.MaxLocalFollowTail, strings.Join(files, ", "))}
	case tail > runtime.MaxLogTail:
		return runtime.MaxLogTail, nil
	}
	return tail, nil
}

// StreamServiceLogs opens a live, line-by-line log stream for a single service
// and wraps each line into a structured LogLine. Because the call is scoped to
// one named service, attribution is exact — the service name comes from the
// argument, not from parsing a prefix — and the backend is chosen by the
// service's runtime. The channel closes when the underlying stream ends or ctx
// is done; cancelling ctx tears down the upstream follow process.
//
// tail, when > 0, replays the last N lines before the live follow begins
// (docker compose / process-compose `--tail`); 0 replays runtime.DefaultLogTail.
// See logTail for its limits.
func (p *Platform) StreamServiceLogs(ctx context.Context, service string, tail int) (<-chan api.LogLine, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	svc, ok := stack.Services[service]
	if !ok {
		return nil, fmt.Errorf("service %q not found", service)
	}
	var local []string
	if svc.Runtime == manifest.RuntimeLocal {
		local = []string{service}
	}
	if tail, err = logTail(tail, local, true); err != nil {
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
