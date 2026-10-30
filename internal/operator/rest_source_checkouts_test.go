package operator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/platformclient"
	"github.com/ang-ee/angee-operator/internal/service"
)

type checkoutObserver struct {
	service.API
	done chan struct{}
}

func (o *checkoutObserver) WithSourceCheckout(ctx context.Context, req api.SourceCheckoutRequest, use func(context.Context, api.SourceCheckout) error) error {
	defer close(o.done)
	return o.API.WithSourceCheckout(ctx, req, use)
}

func TestSourceCheckoutRequiresAdminAndStrictRequest(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: checkout-test\n")
	s, err := NewServer(Config{Root: root, Token: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	minted, err := s.tokens.MintConnection("reader", []string{"*", "admin"}, "1h")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token, body string
		status      int
	}{
		{"", `{}`, http.StatusUnauthorized},
		{"bad", `{}`, http.StatusUnauthorized},
		{minted.Token, `{}`, http.StatusForbidden},
		{"admin", `{"repository":"x","revision":"main","unexpected":true}`, http.StatusBadRequest},
		{"admin", `{} {}`, http.StatusBadRequest},
		{"admin", `{}`, http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/source-checkouts", strings.NewReader(test.body))
		if test.token != "" {
			r.Header.Set("Authorization", "Bearer "+test.token)
		}
		w := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("status = %d, want %d: %s", w.Code, test.status, w.Body.String())
		}
	}
	dev, err := NewServer(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	w := httptest.NewRecorder()
	dev.server.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/source-checkouts", strings.NewReader(`{}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unconfigured admin = %d", w.Code)
	}
}

func sourceCheckoutServer(t *testing.T) (*Server, *httptest.Server, string, string) {
	t.Helper()
	root, repository := t.TempDir(), t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: checkout-test\n")
	runTestGit(t, repository, "init", "-b", "main")
	runTestGit(t, repository, "config", "user.name", "Checkout Test")
	runTestGit(t, repository, "config", "user.email", "checkout@example.com")
	if err := os.WriteFile(filepath.Join(repository, "metadata-input"), []byte("build input"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", ".")
	runTestGit(t, repository, "commit", "-m", "initial")
	want := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	s, err := NewServer(Config{Root: root, Token: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.server.Handler)
	ts.Config = s.server
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close) // Cancel scoped handlers before the test listener waits.
	return s, ts, repository, want
}

func TestSourceCheckoutResponseOwnsFilesUntilDisconnectOrDaemonShutdown(t *testing.T) {
	for _, mode := range []string{"disconnect", "close", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s, ts, repository, want := sourceCheckoutServer(t)
			if s.server.WriteTimeout != 0 {
				t.Fatal("checkout lifetime is bounded by a server write timeout")
			}
			observed := &checkoutObserver{API: s.platform, done: make(chan struct{})}
			s.platform = observed
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			data, _ := json.Marshal(api.SourceCheckoutRequest{Repository: repository, Revision: "main"})
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/source-checkouts", strings.NewReader(string(data)))
			r.Header.Set("Authorization", "Bearer admin")
			resp, err := ts.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
				t.Fatalf("response = %d %v", resp.StatusCode, resp.Header)
			}
			var checkout api.SourceCheckout
			if err := json.NewDecoder(resp.Body).Decode(&checkout); err != nil {
				t.Fatal(err)
			}
			if checkout.Revision != want {
				t.Fatalf("revision = %s, want %s", checkout.Revision, want)
			}
			path := filepath.Join(s.config.Root, checkout.Path)
			if _, err := os.Stat(filepath.Join(path, "metadata-input")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-observed.done:
				t.Fatal("scope ended before its consumer finished")
			default:
			}
			switch mode {
			case "disconnect":
				_ = resp.Body.Close()
			case "close":
				s.Close()
			case "shutdown":
				if err := s.server.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-observed.done:
			case <-ctx.Done():
				t.Fatal("checkout did not clean up", ctx.Err())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("checkout remains: %v", err)
			}
		})
	}
}

func TestSourceCheckoutRemoteClientAuthenticatesAgainstRealOperator(t *testing.T) {
	s, ts, repository, want := sourceCheckoutServer(t)
	observed := &checkoutObserver{API: s.platform, done: make(chan struct{})}
	s.platform = observed
	minted, err := s.tokens.MintConnection("reader", []string{"*", "admin"}, "1h")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		token  string
		status int
	}{{"", http.StatusUnauthorized}, {minted.Token, http.StatusForbidden}, {"admin", http.StatusOK}} {
		called := false
		client := platformclient.New(ts.URL, platformclient.WithBearerToken(test.token))
		err := client.WithSourceCheckout(t.Context(), api.SourceCheckoutRequest{Repository: repository, Revision: "main"}, func(_ context.Context, checkout api.SourceCheckout) error {
			called = true
			if checkout.Revision != want {
				t.Fatalf("revision = %s, want %s", checkout.Revision, want)
			}
			if _, err := os.Stat(filepath.Join(s.config.Root, checkout.Path, "metadata-input")); err != nil {
				t.Fatal(err)
			}
			return nil
		})
		if test.status == http.StatusOK {
			if err != nil || !called {
				t.Fatalf("admin checkout = %v, callback=%v", err, called)
			}
			continue
		}
		var remote *platformclient.RemoteError
		if !errors.As(err, &remote) || remote.Status != test.status || called {
			t.Fatalf("refused checkout = %v, callback=%v", err, called)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-observed.done:
	case <-ctx.Done():
		t.Fatal("authenticated checkout did not clean up")
	}
}
