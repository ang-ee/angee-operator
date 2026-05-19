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

// Mint issues a JWT for actor with the given TTL (parsed as a Go duration;
// empty defaults to 1h, capped at 24h). The token is signed with HMAC-SHA256.
func (m *tokenMinter) Mint(actor, ttlSpec string) (api.ConnectionTokenResponse, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return api.ConnectionTokenResponse{}, errors.New("actor is required")
	}
	ttl := defaultConnectionTokenTTL
	if ttlSpec != "" {
		parsed, err := time.ParseDuration(ttlSpec)
		if err != nil {
			return api.ConnectionTokenResponse{}, fmt.Errorf("parse ttl %q: %w", ttlSpec, err)
		}
		if parsed <= 0 {
			return api.ConnectionTokenResponse{}, errors.New("ttl must be positive")
		}
		if parsed > maxConnectionTokenTTL {
			return api.ConnectionTokenResponse{}, fmt.Errorf("ttl %s exceeds cap of %s", parsed, maxConnectionTokenTTL)
		}
		ttl = parsed
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	claims := jwt.RegisteredClaims{
		Subject:   actor,
		Issuer:    "angee-operator",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expires),
	}
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

// deriveJWTSecret produces a stable JWT signing key from the operator's
// admin bearer using HKDF-like construction via HMAC-SHA256. The derivation
// is one-way, so leaking the JWT secret cannot reveal the admin bearer.
func deriveJWTSecret(adminBearer string) []byte {
	mac := hmac.New(sha256.New, []byte("angee-operator/jwt-derive/v1"))
	mac.Write([]byte(adminBearer))
	return mac.Sum(nil)
}

// fingerprint returns a short, non-reversible identifier for the signing
// key suitable for log messages so operators can confirm two processes
// share the same secret without leaking the secret itself.
func (m *tokenMinter) fingerprint() string {
	sum := sha256.Sum256(m.secret)
	return hex.EncodeToString(sum[:4])
}
