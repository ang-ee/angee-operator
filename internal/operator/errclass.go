package operator

import (
	"log/slog"
	"net/http"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/service"
)

// classifiedError is the single decoded form of a service-layer error, shared
// by the REST (writeServiceError) and GraphQL (formatGraphQLError) surfaces
// so both report the error contract the same way: the code, cause and context
// of a service.OperationError. status is the HTTP status the code maps to.
type classifiedError struct {
	status    int
	code      string
	cause     string
	operation string
	workspace string
	slot      string
	source    string
	remote    string
	job       string
	service   string
	paths     []string
	hint      string
	detail    string
	message   string
}

// classifyServiceError walks the service error taxonomy exactly once. It is the
// one place the error contract is decoded; REST and GraphQL both call it and
// shape their own response from the result.
func classifyServiceError(err error) classifiedError {
	if err == nil {
		return classifiedError{status: http.StatusInternalServerError, code: service.CodeInternal, message: "unknown error"}
	}
	opErr := service.AsOperationError(err)
	c := classifiedError{
		code:      opErr.Code,
		cause:     opErr.Cause,
		operation: opErr.Operation,
		workspace: opErr.Workspace,
		slot:      opErr.Slot,
		source:    opErr.Source,
		remote:    opErr.Remote,
		job:       opErr.Job,
		service:   opErr.Service,
		paths:     opErr.Paths,
		hint:      opErr.Hint,
		detail:    opErr.Detail,
		message:   opErr.Error(),
	}
	// The service redacts where text enters an error (git's output, remote
	// URLs); this is the one pass on the way out, for anything that slipped
	// through, such as a credential in a wrapped error's text.
	for _, field := range []*string{&c.message, &c.remote, &c.detail} {
		*field = logctx.RedactText(*field)
	}
	c.status = statusForCode(c.code)
	return c
}

// statusForCode is the HTTP status a REST error with code is returned with.
func statusForCode(code string) int {
	switch code {
	case service.CodeNotFound:
		return http.StatusNotFound
	case service.CodeInvalidInput:
		return http.StatusBadRequest
	case service.CodeConflict, service.CodePreconditionFailed:
		return http.StatusConflict
	default:
		// GIT_FAILED and TIMEOUT stay 500 as before: a 502 or 504 from the
		// operator would read as a gateway failure to proxies in front of it,
		// which may retry the request or replace the body.
		return http.StatusInternalServerError
	}
}

// codeUnauthorized is the code of a REST request rejected for its
// credentials, before it reaches an operation.
const codeUnauthorized = "UNAUTHORIZED"

// codeForStatus is the code of a REST error written without a service error,
// such as a malformed request or a missing token.
func codeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return codeUnauthorized
	case status == http.StatusNotFound:
		return service.CodeNotFound
	case status == http.StatusConflict:
		return service.CodeConflict
	case status >= 400 && status < 500:
		return service.CodeInvalidInput
	default:
		return service.CodeInternal
	}
}

// errorResponse is the REST body for c.
func (c classifiedError) errorResponse() api.ErrorResponse {
	return api.ErrorResponse{
		Error:     c.message,
		Code:      c.code,
		Cause:     c.cause,
		Operation: c.operation,
		Workspace: c.workspace,
		Slot:      c.slot,
		Source:    c.source,
		Remote:    c.remote,
		Job:       c.job,
		Service:   c.service,
		Paths:     c.paths,
		Hint:      c.hint,
		Detail:    c.detail,
	}
}

// extensions are the GraphQL error extensions for c: code always, and every
// other key only when it has a value.
func (c classifiedError) extensions() map[string]any {
	ext := map[string]any{"code": c.code}
	for key, value := range map[string]string{
		"cause":     c.cause,
		"operation": c.operation,
		"workspace": c.workspace,
		"slot":      c.slot,
		"source":    c.source,
		"remote":    c.remote,
		"job":       c.job,
		"service":   c.service,
		"hint":      c.hint,
		"detail":    c.detail,
	} {
		if value != "" {
			ext[key] = value
		}
	}
	if len(c.paths) > 0 {
		ext["paths"] = c.paths
	}
	return ext
}

// logLevel is the level a failed operation is logged at: WARN for a failure
// the client can act on, ERROR for one in git, a job or the operator.
func (c classifiedError) logLevel() slog.Level {
	switch c.code {
	case service.CodeNotFound, service.CodeInvalidInput, service.CodeConflict, service.CodePreconditionFailed:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

// logAttrs are the attributes a failed operation's log record carries.
func (c classifiedError) logAttrs() []slog.Attr {
	attrs := []slog.Attr{slog.String("code", c.code)}
	if c.cause != "" {
		attrs = append(attrs, slog.String("cause", c.cause))
	}
	if c.operation != "" {
		attrs = append(attrs, slog.String("operation", c.operation))
	}
	return append(attrs, slog.String("error", c.message))
}
