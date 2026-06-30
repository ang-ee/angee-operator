// Package store is the lowest storage primitive in angee-operator: a generic
// key->bytes backend modeled on Vault's physical.Backend. It carries ZERO
// domain knowledge and is a leaf package (stdlib only) so that higher layers
// such as internal/secrets and the files object layer can depend on it without
// risking an import cycle (secrets imports store, never the reverse).
package store

import (
	"context"
	"errors"
)

// Blob is the small value type Store operates over. Etag is an opaque version
// token (sha256 hex for localfs); backends that do not version leave it empty.
type Blob struct {
	Bytes []byte
	Etag  string
}

// Store is the MINIMAL generic key->bytes backend. It carries ZERO domain
// knowledge: no secret verbs (rotate/lease/renew/revoke/transit/version), no
// file verbs. Modeled on Vault's physical.Backend. Keep it tiny; the bigger the
// interface the weaker the abstraction.
type Store interface {
	Get(ctx context.Context, key string) (Blob, bool, error)
	Set(ctx context.Context, key string, value Blob) error
}

// The following are OPTIONAL capability interfaces, discovered by TYPE
// ASSERTION (the io / database/sql idiom). Consumers assert for exactly what
// they need; the core Store never widens to absorb them.

// Lister enumerates the keys a store currently holds.
type Lister interface {
	List(ctx context.Context) ([]string, error)
}

// Deleter removes a key from a store.
type Deleter interface {
	Delete(ctx context.Context, key string) error
}

// Versioned adds compare-and-set semantics over Etag. A write with a non-empty
// expectedEtag must fail with ErrEtagMismatch when the current stored Etag
// differs; an empty expectedEtag is an unconditional write. localfs implements
// this; env-file and openbao deliberately do NOT (last-write-wins — files never
// run on them).
type Versioned interface {
	SetIf(ctx context.Context, key string, value Blob, expectedEtag string) (Blob, error)
}

// Sentinel errors. The object layers above map these into their own typed
// errors (ErrEtagMismatch -> ConflictError, ErrNotContained -> InvalidInput).
var (
	// ErrEtagMismatch is returned by Versioned.SetIf when the supplied
	// expectedEtag does not match the currently stored Etag.
	ErrEtagMismatch = errors.New("store: etag mismatch")
	// ErrNotContained is returned when a key resolves outside the backend root.
	ErrNotContained = errors.New("store: path escapes root")
	// ErrTooLarge is returned when an on-disk value exceeds a backend's
	// configured MaxBytes cap. localfs enforces this BEFORE reading the file
	// into memory so the cap bounds allocation rather than rejecting after it.
	ErrTooLarge = errors.New("store: value exceeds size cap")
)

// RESERVED secrets-engine seam (documented, intentionally NOT built).
//
// If rotation, leases, dynamic credentials, transit encryption, or no-readback
// semantics are ever required, they belong in the *secrets object layer* behind
// its own request-oriented interface (à la Vault's logical.Backend) — NEVER as
// Store methods, NEVER via a gocloud-style As() escape hatch, and NEVER as a
// storage capability interface in this package. Store stays a dumb key->bytes
// primitive. This comment is the marker; there is deliberately no code here.
