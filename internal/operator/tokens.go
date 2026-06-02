package operator

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultConnectionTokenTTL = time.Hour
	maxConnectionTokenTTL     = 24 * time.Hour
)

const (
	tokenIssuer = "angee-operator"
	// audienceOperator scopes a token to the operator's own API
	// (the two-tier auth middleware and, later, the WebSocket InitFunc).
	audienceOperator = "operator"
	// audienceServicePrefix scopes a token to a single routed service's
	// socket through the edge: aud = "svc:<service-name>".
	audienceServicePrefix = "svc:"
)

// serviceAudience returns the audience value for a route token bound to the
// named service.
func serviceAudience(service string) string {
	return audienceServicePrefix + service
}

// Claims is the operator's JWT claim set: the standard registered claims
// (sub=actor, iss=angee-operator, exp, iat, aud) plus an optional capability
// scope carried on operator-API tokens. The audience uses the standard `aud`
// claim so jwt's WithAudience validation can enforce it.
//
// Scope is advisory at this phase: the auth middleware attaches it to the
// request context (see withActorScope) but no path enforces it yet, so the
// presence of a scope does not currently restrict access.
type Claims struct {
	jwt.RegisteredClaims
	Scope []string `json:"scope,omitempty"`
}

// tokenMinter signs short-lived connection tokens scoped to a specific
// actor. The signing material is either an explicit operator-configured
// secret (preferred for production) or, in dev, an HKDF-derived value
// from the admin bearer; in pure-loopback dev with no bearer, a
// per-process random key is generated so tokens stay valid for the
// operator's lifetime and become opaque to anyone else.
type tokenMinter struct {
	secret []byte
}

func newTokenMinter(explicitSecret, adminBearer string) (*tokenMinter, error) {
	switch {
	case explicitSecret != "":
		return &tokenMinter{secret: []byte(explicitSecret)}, nil
	case adminBearer != "":
		key := deriveJWTSecret(adminBearer)
		return &tokenMinter{secret: key}, nil
	default:
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("generate per-process JWT secret: %w", err)
		}
		return &tokenMinter{secret: buf}, nil
	}
}

// MintConnection issues an operator-API token (aud="operator") carrying the
// given capability scope, which the auth layer will gate mutations against.
// This is what the host backend mints (server-side, over the admin bearer)
// and hands to a browser instead of the admin bearer itself.
func (m *tokenMinter) MintConnection(actor string, scope []string, ttlSpec string) (api.ConnectionTokenResponse, error) {
	return m.mint(actor, audienceOperator, scope, ttlSpec)
}

// MintRoute issues a route token (aud="svc:<service>", no scope) that
// authorizes opening that one service's socket through the edge.
func (m *tokenMinter) MintRoute(actor, service, ttlSpec string) (api.ConnectionTokenResponse, error) {
	service = strings.TrimSpace(service)
	if service == "" {
		return api.ConnectionTokenResponse{}, errors.New("service is required")
	}
	return m.mint(actor, serviceAudience(service), nil, ttlSpec)
}

// mint signs an HMAC-SHA256 JWT for actor with the given audience, scope, and
// TTL (parsed as a Go duration; empty defaults to 1h, capped at 24h).
func (m *tokenMinter) mint(actor, audience string, scope []string, ttlSpec string) (api.ConnectionTokenResponse, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return api.ConnectionTokenResponse{}, errors.New("actor is required")
	}
	if audience == "" {
		// Every minted token carries exactly one audience; an audience-less
		// token could never satisfy Verify and is a programming error.
		return api.ConnectionTokenResponse{}, errors.New("audience is required")
	}
	ttl, err := parseTokenTTL(ttlSpec)
	if err != nil {
		return api.ConnectionTokenResponse{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   actor,
			Issuer:    tokenIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
		Scope: scope,
	}
	claims.Audience = jwt.ClaimStrings{audience}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(m.secret)
	if err != nil {
		return api.ConnectionTokenResponse{}, fmt.Errorf("sign token: %w", err)
	}
	return api.ConnectionTokenResponse{
		Token:     signed,
		Actor:     actor,
		ExpiresAt: expires.Format(time.RFC3339Nano),
	}, nil
}

// parseTokenTTL validates and resolves a TTL spec to a duration.
func parseTokenTTL(ttlSpec string) (time.Duration, error) {
	if ttlSpec == "" {
		return defaultConnectionTokenTTL, nil
	}
	parsed, err := time.ParseDuration(ttlSpec)
	if err != nil {
		return 0, fmt.Errorf("parse ttl %q: %w", ttlSpec, err)
	}
	if parsed <= 0 {
		return 0, errors.New("ttl must be positive")
	}
	if parsed > maxConnectionTokenTTL {
		return 0, fmt.Errorf("ttl %s exceeds cap of %s", parsed, maxConnectionTokenTTL)
	}
	return parsed, nil
}

// Verify parses and validates raw against the minter's signing key, requiring
// HS256, the operator issuer, a present expiry, and membership of wantAudience
// in the token's audience. It is the single verifier shared by the operator-API
// auth middleware and the edge forward_auth target. On success it returns the
// token's claims (actor in Subject, capabilities in Scope).
func (m *tokenMinter) Verify(raw, wantAudience string) (Claims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Claims{}, errors.New("empty token")
	}
	if wantAudience == "" {
		// Fail closed: an empty expected audience means the caller failed to
		// resolve which audience it requires (e.g. a missing service name in
		// the edge forward_auth target), not "accept any audience".
		return Claims{}, errors.New("audience is required")
	}
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return m.secret, nil
	},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithIssuedAt(),
		jwt.WithExpirationRequired(),
		jwt.WithAudience(wantAudience),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("verify token: %w", err)
	}
	return claims, nil
}

// deriveJWTSecret produces a stable JWT signing key from the operator's
// admin bearer using HKDF-like construction via HMAC-SHA256. The derivation
// is one-way, so leaking the JWT secret cannot reveal the admin bearer.
func deriveJWTSecret(adminBearer string) []byte {
	mac := hmac.New(sha256.New, []byte("angee-operator/jwt-derive/v1"))
	mac.Write([]byte(adminBearer))
	return mac.Sum(nil)
}

// Fingerprint returns a short, non-reversible identifier for the signing
// key suitable for log messages so operators can confirm two processes
// share the same secret without leaking the secret itself.
func (m *tokenMinter) Fingerprint() string {
	sum := sha256.Sum256(m.secret)
	return hex.EncodeToString(sum[:4])
}
