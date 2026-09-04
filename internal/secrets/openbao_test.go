package secrets

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenBaoRequestTracesRedactedURLWithoutToken(t *testing.T) {
	const token = "vault-secret-token"
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	backend := NewOpenBaoBackend(OpenBaoConfig{
		Address: "https://user:password@openbao.invalid",
		Token:   token,
	})
	backend.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-Vault-Token"); got != token {
			t.Fatalf("X-Vault-Token = %q, want configured token", got)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})}
	if err := backend.Delete(ctx, "api-key"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "http DELETE https://***@openbao.invalid/v1/secret/data/angee/api-key") ||
		!strings.Contains(got, "http finished status=204 duration=") {
		t.Fatalf("trace output = %q", got)
	}
	for _, secret := range []string{"user", "password", token} {
		if strings.Contains(got, secret) {
			t.Fatalf("trace output leaked %q: %q", secret, got)
		}
	}
}
