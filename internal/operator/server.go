// Package operator implements the HTTP API and MCP server for the angee operator.
// All business logic lives in internal/service — this package is a thin adapter.
package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
)

// Server defaults: tight enough to defeat slowloris, loose enough to allow
// large compose YAMLs and long-running log/exec streams (the streaming
// handlers manage their own deadlines via r.Context()).
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 60 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultMaxHeaderBytes    = 1 << 20 // 1 MiB
	defaultMaxBodyBytes      = 4 << 20 // 4 MiB — angee.yaml is never this big
)

// Server is the angee operator HTTP server.
// It delegates all business logic to Platform.
type Server struct {
	Platform *service.Platform
	Log      *slog.Logger
}

// New creates an operator Server for the given ANGEE_ROOT path.
func New(angeeRoot string, logger *slog.Logger) (*Server, error) {
	plat, err := service.NewPlatform(angeeRoot, logger)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = plat.Log
	}
	return &Server{Platform: plat, Log: logger}, nil
}

// Handler returns the HTTP handler for the operator API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /health", s.handleHealth)

	// Config
	mux.HandleFunc("GET /config", s.handleConfigGet)
	mux.HandleFunc("POST /config", s.handleConfigSet)

	// Provisioning
	mux.HandleFunc("POST /stacks/init", s.handleStackInit)
	mux.HandleFunc("POST /stacks/update", s.handleStackUpdate)
	mux.HandleFunc("POST /workspaces/init", s.handleWorkspaceInit)
	mux.HandleFunc("POST /workspaces/list", s.handleWorkspaceList)
	mux.HandleFunc("POST /workspaces/{name}/update", s.handleWorkspaceUpdate)
	mux.HandleFunc("POST /workspaces/{name}/dev", s.handleWorkspaceDev)
	mux.HandleFunc("POST /agents/init", s.handleAgentInit)
	mux.HandleFunc("POST /agents/{name}/update", s.handleAgentUpdate)
	mux.HandleFunc("POST /agents/{name}/restart", s.handleAgentRestart)
	mux.HandleFunc("POST /agents/{name}/destroy", s.handleAgentDestroy)
	mux.HandleFunc("POST /agents/{name}/chat", s.handleAgentChat)
	mux.HandleFunc("POST /agents/{name}/ask", s.handleAgentAsk)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)

	// Deployment lifecycle
	mux.HandleFunc("POST /deploy", s.handleDeploy)
	mux.HandleFunc("POST /rollback", s.handleRollback)
	mux.HandleFunc("POST /pull", s.handlePull)
	mux.HandleFunc("GET /plan", s.handlePlan)

	// Runtime status
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /logs/{service}", s.handleLogs)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("POST /scale/{service}", s.handleScale)
	mux.HandleFunc("POST /down", s.handleDown)

	// Agent operations
	mux.HandleFunc("GET /agents", s.handleAgentList)
	mux.HandleFunc("POST /agents/{name}/start", s.handleAgentStart)
	mux.HandleFunc("POST /agents/{name}/stop", s.handleAgentStop)
	mux.HandleFunc("GET /agents/{name}/logs", s.handleAgentLogs)

	// Git history
	mux.HandleFunc("GET /history", s.handleHistory)

	// MCP endpoint
	mux.HandleFunc("POST /mcp", s.handleMCP)

	// OpenAPI
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)

	apiKey := s.Platform.APIKey()
	origins := s.Platform.Cfg.CORSOrigins

	// Order: recovery → logging → CORS → auth → bodyLimit → mux.
	// Recovery is outermost so panics in any layer are caught and logged.
	// Body-limit is innermost so OPTIONS preflight (which has no body) and
	// auth-rejected requests don't waste cycles wrapping the reader.
	return recoveryMiddleware(
		loggingMiddleware(
			corsMiddleware(
				authMiddleware(
					bodyLimitMiddleware(mux, defaultMaxBodyBytes),
					apiKey, s.Log,
				),
				origins,
			),
			s.Log,
		),
		s.Log,
	)
}

// Start runs the operator HTTP server.
//
// It refuses to start when bound to a non-loopback address without an API
// key configured. This prevents the common deployment mistake of exposing
// /config and /deploy endpoints to the network unauthenticated.
func (s *Server) Start(ctx context.Context, addr string) error {
	if !s.Platform.Cfg.IsLoopbackBind() && s.Platform.APIKey() == "" {
		return fmt.Errorf(
			"operator refusing to bind to %s without an API key; "+
				"set api_key in operator.yaml (or ANGEE_API_KEY) before binding to a non-loopback address",
			s.Platform.Cfg.BindAddress,
		)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		// WriteTimeout intentionally unset — /logs and /agents/{name}/logs
		// stream long-lived responses; cancellation is driven by r.Context().
		IdleTimeout:    defaultIdleTimeout,
		MaxHeaderBytes: defaultMaxHeaderBytes,
	}
	s.Log.Info("operator started", "addr", addr, "root", s.Platform.Root.Path, "runtime", s.Platform.Cfg.Runtime)
	s.Platform.RestartHealthProbes(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ── Response helpers ────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, err error) {
	code := service.ErrorStatus(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: err.Error()}) //nolint:errcheck
}

// ── Middleware ───────────────────────────────────────────────────────────────

func authMiddleware(next http.Handler, apiKey string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" || r.URL.Path == "/health" || r.URL.Path == "/openapi.json" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != apiKey {
			log.Warn("unauthorized request", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware catches panics from downstream handlers and emits a
// 500 response. If the handler already started writing the response we can't
// safely override the status, so we just log.
func recoveryMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic recovered", "error", rec, "stack", string(debug.Stack()))
				if !sr.headerWritten {
					writeError(sr, &service.ServiceError{Status: 500, Message: "internal server error"})
				}
			}
		}()
		next.ServeHTTP(sr, r)
	})
}

func corsMiddleware(next http.Handler, origins []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && matchOrigin(origin, origins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Only emit allow-methods/headers when the origin is authorised;
			// browsers ignore them otherwise and they leak request shape.
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly is `^[a-z0-9-]+$` and is used to check the suffix that survives
// after we've stripped a wildcard prefix from a CORS pattern. Without this
// check `http://localhost:*` would match `http://localhost:1234.evil.com`.
var hostOnly = regexp.MustCompile(`^[A-Za-z0-9.\-:]+$`)

func matchOrigin(origin string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			prefix := strings.TrimSuffix(p, "*")
			if !strings.HasPrefix(origin, prefix) {
				continue
			}
			// What comes after the prefix must look like a port/host segment
			// — letters, digits, dots, dashes, colons. A path or `.evil.com`
			// suffix should not match a `localhost:*` pattern.
			suffix := origin[len(prefix):]
			if suffix == "" || hostOnly.MatchString(suffix) {
				return true
			}
		} else if origin == p {
			return true
		}
	}
	return false
}

// statusRecorder wraps a ResponseWriter and remembers the status code +
// whether the header has been written. Used by both logging and recovery.
type statusRecorder struct {
	http.ResponseWriter
	code          int
	headerWritten bool
	bytes         int
}

func (s *statusRecorder) WriteHeader(c int) {
	if s.headerWritten {
		return
	}
	s.code = c
	s.headerWritten = true
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.headerWritten {
		s.headerWritten = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush makes statusRecorder usable with streaming handlers (e.g. /logs).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func loggingMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr, ok := w.(*statusRecorder)
		if !ok {
			sr = &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		}
		start := time.Now()
		next.ServeHTTP(sr, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.code,
			"bytes", sr.bytes,
			"dur_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// bodyLimitMiddleware caps incoming request bodies. Streaming endpoints
// like /logs are GETs and don't have bodies, so they're unaffected.
func bodyLimitMiddleware(next http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.ContentLength != 0 {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}
