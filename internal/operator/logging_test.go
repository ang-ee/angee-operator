package operator

import (
	"bufio"
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestServerLevelFromCount(t *testing.T) {
	for _, test := range []struct {
		count int
		want  slog.Level
	}{
		{count: -1, want: slog.LevelInfo},
		{count: 0, want: slog.LevelInfo},
		{count: 1, want: slog.LevelDebug},
		{count: 2, want: slog.LevelDebug},
		{count: 8, want: slog.LevelDebug},
	} {
		if got := serverLevelFromCount(test.count); got != test.want {
			t.Errorf("serverLevelFromCount(%d) = %v, want %v", test.count, got, test.want)
		}
	}
}

func TestRequestLoggingCarriesRequestLoggerAndRecordsCompletion(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(logctx.NewServerHandler(&output, slog.LevelDebug))
	s := &Server{config: Config{Logger: logger}}
	handler := s.requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logctx.From(r.Context()).InfoContext(r.Context(), "handler reached")
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/widgets?token=must-not-appear", nil)
	req.RemoteAddr = "192.0.2.10:4321"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	got := output.String()
	for _, want := range []string{
		"level=DEBUG msg=\"request started\"",
		"level=INFO msg=request",
		"method=POST",
		"path=/widgets",
		"status=201",
		"duration=",
		"remote=192.0.2.10:4321",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("request output = %q, want %q", got, want)
		}
	}
	match := regexp.MustCompile(`req=([0-9a-f]{8})`).FindStringSubmatch(got)
	if len(match) != 2 {
		t.Fatalf("request output = %q, want an eight-character hex request ID", got)
	}
	if !strings.Contains(got, "msg=\"handler reached\" req="+match[1]) {
		t.Fatalf("handler log did not carry request ID %q: %q", match[1], got)
	}
	for _, forbidden := range []string{"must-not-appear", "token="} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("request output leaked query material %q: %q", forbidden, got)
		}
	}
}

func TestHealthRequestDoesNotLogAtInfo(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(logctx.NewServerHandler(&output, slog.LevelInfo))
	s := &Server{config: Config{Logger: logger}}
	handler := s.requestLogging(http.HandlerFunc(s.health))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if output.Len() != 0 {
		t.Fatalf("health request logged at info: %q", output.String())
	}
}

func TestSecretAuditUsesRequestLogger(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(logctx.NewServerHandler(&output, slog.LevelInfo)).With("req", "deadbeef")
	req := httptest.NewRequest(http.MethodPost, "/secrets/db-password", nil)
	req.RemoteAddr = "192.0.2.20:1234"
	req = req.WithContext(logctx.With(req.Context(), logger))

	auditSecretMutation(req, "set", "db-password")

	got := output.String()
	for _, want := range []string{
		"level=INFO",
		"msg=\"secret mutation\"",
		"req=deadbeef",
		"operation=set",
		"name=db-password",
		"remote=192.0.2.20:1234",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("audit output = %q, want %q", got, want)
		}
	}
}

func TestAuthLogsResolvedActorAtDebug(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(logctx.NewServerHandler(&output, slog.LevelDebug))
	minter, err := newTokenMinter("explicit-jwt-secret-1234", "admin-bearer")
	if err != nil {
		t.Fatalf("newTokenMinter() error = %v", err)
	}
	token, err := minter.MintConnection("alice", []string{"service:read"}, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}
	s := &Server{config: Config{Token: "admin-bearer", Logger: logger}, tokens: minter}
	handler := s.requestLogging(s.auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	req.Header.Set("Authorization", "Bearer "+token.Token)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	got := output.String()
	for _, want := range []string{
		"level=DEBUG msg=authenticated",
		"actor=alice",
		"scope=[service:read]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("auth output = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, token.Token) {
		t.Fatalf("auth output leaked bearer token: %q", got)
	}
}

type hijackingRecorder struct {
	*httptest.ResponseRecorder
}

func (*hijackingRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

func TestStatusResponseWriterPreservesStreamingInterfaces(t *testing.T) {
	base := &hijackingRecorder{ResponseRecorder: httptest.NewRecorder()}
	wrapped, status := wrapStatusResponseWriter(base)
	flusher, flushes := wrapped.(http.Flusher)
	_, hijacks := wrapped.(http.Hijacker)
	if !flushes || !hijacks {
		t.Fatalf("wrapped writer interfaces: flusher=%t hijacker=%t, want both", flushes, hijacks)
	}
	flusher.Flush()
	if status.statusCode() != http.StatusOK {
		t.Fatalf("status after flush = %d, want 200", status.statusCode())
	}

	wrapped, status = wrapStatusResponseWriter(base)
	hijacker := wrapped.(http.Hijacker)
	if _, _, err := hijacker.Hijack(); err != nil {
		t.Fatalf("Hijack() error = %v", err)
	}
	if status.statusCode() != http.StatusSwitchingProtocols {
		t.Fatalf("status after hijack = %d, want 101", status.statusCode())
	}
}
