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
	resp, err := m.MintConnection("user-1", nil, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
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
	resp, err := m.MintConnection("svc", nil, "")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
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
			_, err := m.MintConnection(actor, nil, tc.ttl)
			if err == nil {
				t.Fatalf("MintConnection() error = nil, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MintConnection() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestMintStampsOperatorAudience(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	resp, err := m.MintConnection("user-1", nil, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}
	claims, err := m.Verify(resp.Token, audienceOperator)
	if err != nil {
		t.Fatalf("Verify(operator) error = %v", err)
	}
	if claims.Subject != "user-1" {
		t.Fatalf("Subject = %q, want user-1", claims.Subject)
	}
	if len(claims.Scope) != 0 {
		t.Fatalf("Scope = %#v, want empty", claims.Scope)
	}
}

func TestVerifyRejectsWrongAudience(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	// A route token (aud=svc:agent-x) must not satisfy an operator-API check.
	resp, err := m.MintRoute("actor", "agent-x", "1h")
	if err != nil {
		t.Fatalf("MintRoute() error = %v", err)
	}
	if _, err := m.Verify(resp.Token, audienceOperator); err == nil {
		t.Fatal("Verify(operator) on a svc token error = nil, want audience rejection")
	}
	if _, err := m.Verify(resp.Token, serviceAudience("agent-x")); err != nil {
		t.Fatalf("Verify(svc:agent-x) error = %v, want success", err)
	}
	if _, err := m.Verify(resp.Token, serviceAudience("agent-y")); err == nil {
		t.Fatal("Verify(svc:agent-y) error = nil, want audience rejection")
	}
	// An empty expected audience must fail closed, not match any token.
	if _, err := m.Verify(resp.Token, ""); err == nil {
		t.Fatal("Verify(\"\") error = nil, want audience-required rejection")
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "actor",
			Issuer:    tokenIssuer,
			Audience:  jwt.ClaimStrings{audienceOperator},
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	})
	raw, err := expired.SignedString(m.secret)
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	if _, err := m.Verify(raw, audienceOperator); err == nil {
		t.Fatal("Verify() on expired token error = nil, want expiry rejection")
	}
}

func TestVerifyRejectsTamperedAndForeignTokens(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	other, _ := newTokenMinter("a-different-secret", "")

	resp, err := m.MintConnection("actor", nil, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}
	// A token signed by a different key must not verify here.
	foreign, err := other.MintConnection("actor", nil, "1h")
	if err != nil {
		t.Fatalf("other.MintConnection() error = %v", err)
	}
	if _, err := m.Verify(foreign.Token, audienceOperator); err == nil {
		t.Fatal("Verify() on foreign-signed token error = nil, want signature rejection")
	}
	// Flipping the final signature byte must invalidate the token.
	tampered := resp.Token[:len(resp.Token)-1]
	if resp.Token[len(resp.Token)-1] == 'a' {
		tampered += "b"
	} else {
		tampered += "a"
	}
	if _, err := m.Verify(tampered, audienceOperator); err == nil {
		t.Fatal("Verify() on tampered token error = nil, want signature rejection")
	}
	if _, err := m.Verify("", audienceOperator); err == nil {
		t.Fatal("Verify() on empty token error = nil, want rejection")
	}
}

func TestMintConnectionCarriesScope(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	scope := []string{"service:read", "workspace:create"}
	resp, err := m.MintConnection("alice", scope, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}
	claims, err := m.Verify(resp.Token, audienceOperator)
	if err != nil {
		t.Fatalf("Verify(operator) error = %v", err)
	}
	if claims.Subject != "alice" {
		t.Fatalf("Subject = %q, want alice", claims.Subject)
	}
	if strings.Join(claims.Scope, ",") != strings.Join(scope, ",") {
		t.Fatalf("Scope = %#v, want %#v", claims.Scope, scope)
	}
}

func TestMintRouteAudienceAndValidation(t *testing.T) {
	m, _ := newTokenMinter("secret-abcdefghij", "")
	resp, err := m.MintRoute("alice", "agent-x", "1h")
	if err != nil {
		t.Fatalf("MintRoute() error = %v", err)
	}
	// Bound to exactly that service's audience.
	if _, err := m.Verify(resp.Token, serviceAudience("agent-x")); err != nil {
		t.Fatalf("Verify(svc:agent-x) error = %v", err)
	}
	if _, err := m.Verify(resp.Token, audienceOperator); err == nil {
		t.Fatal("route token verified as operator; want audience rejection")
	}
	// A route token carries no scope.
	claims, _ := m.Verify(resp.Token, serviceAudience("agent-x"))
	if len(claims.Scope) != 0 {
		t.Fatalf("Scope = %#v, want empty", claims.Scope)
	}
	// Empty/whitespace service is rejected.
	if _, err := m.MintRoute("alice", "   ", "1h"); err == nil {
		t.Fatal("MintRoute(empty service) error = nil, want rejection")
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
