package operator

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestTokenMinterUsesExplicitSecret(t *testing.T) {
	m, err := newTokenMinter("explicit-secret-12345", "ignored-bearer")
	if err != nil {
		t.Fatalf("newTokenMinter() error = %v", err)
	}
	resp, err := m.Mint("user-1", "1h")
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if resp.Actor != "user-1" {
		t.Fatalf("Actor = %q, want user-1", resp.Actor)
	}
	// Token must verify against the explicit secret, not the bearer.
	claims, err := parseToken(t, resp.Token, []byte("explicit-secret-12345"))
	if err != nil {
		t.Fatalf("parse with explicit secret error = %v", err)
	}
	if claims.Subject != "user-1" || claims.Issuer != "angee-operator" {
		t.Fatalf("claims = %+v, want sub=user-1 iss=angee-operator", claims)
	}
}

func TestTokenMinterDerivesFromBearer(t *testing.T) {
	m, err := newTokenMinter("", "admin-bearer-xyz")
	if err != nil {
		t.Fatalf("newTokenMinter() error = %v", err)
	}
	resp, err := m.Mint("svc", "")
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	// Default TTL is 1h, exact match within a second.
	expires, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("parse ExpiresAt: %v", err)
	}
	delta := time.Until(expires)
	if delta < 59*time.Minute || delta > 61*time.Minute {
		t.Fatalf("expires delta = %s, want ~1h", delta)
	}
	// HKDF-derived secret should verify the token.
	derived := deriveJWTSecret("admin-bearer-xyz")
	if _, err := parseToken(t, resp.Token, derived); err != nil {
		t.Fatalf("parse with derived secret error = %v", err)
	}
}

func TestTokenMinterRandomDevSecret(t *testing.T) {
	m, err := newTokenMinter("", "")
	if err != nil {
		t.Fatalf("newTokenMinter() error = %v", err)
	}
	if len(m.secret) != 32 {
		t.Fatalf("random secret length = %d, want 32", len(m.secret))
	}
}

func TestTokenMinterRejectsBadInput(t *testing.T) {
	m, _ := newTokenMinter("any", "any")
	cases := []struct {
		name string
		ttl  string
		want string
	}{
		{"empty actor", "1h", "actor is required"},
		{"bad ttl", "huh", "parse ttl"},
		{"zero ttl", "0s", "must be positive"},
		{"too-long ttl", "48h", "exceeds cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor := "u"
			if tc.name == "empty actor" {
				actor = ""
			}
			_, err := m.Mint(actor, tc.ttl)
			if err == nil {
				t.Fatalf("Mint() error = nil, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Mint() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func parseToken(t *testing.T, token string, secret []byte) (*jwt.RegisteredClaims, error) {
	t.Helper()
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	return claims, err
}
