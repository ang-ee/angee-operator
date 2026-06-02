package operator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// authProbe wraps s.auth around a handler that records what the auth layer let
// through: whether it was called, and the actor/scope it resolved onto the
// request context.
type authProbe struct {
	called bool
	actor  string
	scoped bool
}

func (p *authProbe) serve(s *Server, authorization string) *httptest.ResponseRecorder {
	p.called, p.actor, p.scoped = false, "", false
	handler := s.auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p.called = true
		if scope, ok := actorScopeFromContext(r.Context()); ok {
			p.scoped = true
			p.actor = scope.Actor
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/services", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestAuthTwoTier(t *testing.T) {
	const adminBearer = "admin-bearer-secret"
	minter, err := newTokenMinter("explicit-jwt-secret-1234", adminBearer)
	if err != nil {
		t.Fatalf("newTokenMinter() error = %v", err)
	}
	s := &Server{config: Config{Token: adminBearer}, tokens: minter}

	operatorTok, err := minter.MintScoped("alice", audienceOperator, []string{"service:read"}, "1h")
	if err != nil {
		t.Fatalf("MintScoped(operator) error = %v", err)
	}
	routeTok, err := minter.MintScoped("alice", serviceAudience("agent-x"), nil, "1h")
	if err != nil {
		t.Fatalf("MintScoped(svc) error = %v", err)
	}

	probe := &authProbe{}

	t.Run("admin bearer gets full unscoped access", func(t *testing.T) {
		rr := probe.serve(s, "Bearer "+adminBearer)
		if rr.Code != http.StatusOK || !probe.called {
			t.Fatalf("code = %d called = %v, want 200/true", rr.Code, probe.called)
		}
		if probe.scoped {
			t.Fatal("admin bearer should carry no actor scope (full access)")
		}
	})

	t.Run("minted operator token passes with actor in context", func(t *testing.T) {
		rr := probe.serve(s, "Bearer "+operatorTok.Token)
		if rr.Code != http.StatusOK || !probe.called {
			t.Fatalf("code = %d called = %v, want 200/true", rr.Code, probe.called)
		}
		if !probe.scoped || probe.actor != "alice" {
			t.Fatalf("actor scope = %v/%q, want true/alice", probe.scoped, probe.actor)
		}
	})

	t.Run("route token is rejected on the operator API", func(t *testing.T) {
		rr := probe.serve(s, "Bearer "+routeTok.Token)
		if rr.Code != http.StatusUnauthorized || probe.called {
			t.Fatalf("code = %d called = %v, want 401/false", rr.Code, probe.called)
		}
	})

	t.Run("garbage token and missing header are rejected", func(t *testing.T) {
		if rr := probe.serve(s, "Bearer not-a-jwt"); rr.Code != http.StatusUnauthorized || probe.called {
			t.Fatalf("garbage: code = %d called = %v, want 401/false", rr.Code, probe.called)
		}
		if rr := probe.serve(s, ""); rr.Code != http.StatusUnauthorized || probe.called {
			t.Fatalf("missing: code = %d called = %v, want 401/false", rr.Code, probe.called)
		}
	})
}

func TestAuthOpenWhenNoTokenConfigured(t *testing.T) {
	minter, _ := newTokenMinter("", "")
	s := &Server{config: Config{Token: ""}, tokens: minter}
	probe := &authProbe{}
	rr := probe.serve(s, "")
	if rr.Code != http.StatusOK || !probe.called {
		t.Fatalf("code = %d called = %v, want 200/true (open dev mode)", rr.Code, probe.called)
	}
	if probe.scoped {
		t.Fatal("open dev mode should carry no actor scope")
	}
}
