// Package files is the object layer over a store.Store: it reads and writes
// raw UTF-8 text blobs at a relative path under a single rooted store, with a
// size cap and optimistic-concurrency (etag) writes. It carries no YAML or
// other domain knowledge — it deals in raw bytes only. The service layer maps
// the sentinel errors returned here into its own typed errors.
package files

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/ang-ee/angee-operator/internal/store"
)

// MaxFileBytes caps the size of a single file read or written through this
// object layer. It matches the REST body cap (1 MiB).
const MaxFileBytes = 1 << 20

// Sentinel errors. The service layer translates these into service.*Error.
var (
	// ErrNotFound is returned by Read when the relpath has no stored value.
	ErrNotFound = errors.New("files: not found")
	// ErrEtagMismatch is returned by Write when the supplied etag does not
	// match the currently stored version.
	ErrEtagMismatch = errors.New("files: etag mismatch")
	// ErrNotContained is returned when the relpath escapes the store root.
	ErrNotContained = errors.New("files: path escapes root")
	// ErrNotText is returned when content is not valid UTF-8 text.
	ErrNotText = errors.New("files: content is not valid UTF-8 text")
	// ErrTooLarge is returned when content exceeds MaxFileBytes.
	ErrTooLarge = errors.New("files: content exceeds size cap")
	// ErrNotVersioned is returned by New when the store lacks the Versioned
	// capability required for compare-and-set writes.
	ErrNotVersioned = errors.New("files: store does not support versioning")
)

// Content is the object-layer read result for one file.
type Content struct {
	Path    string
	Content string
	Etag    string
}

// Ref is the object-layer metadata result of a write.
type Ref struct {
	Path string
	Etag string
}

// Object is a raw-bytes read/write façade over a Versioned store.
type Object struct {
	store     store.Store
	versioned store.Versioned
}

// New requires the Versioned capability and errors with ErrNotVersioned if the
// store lacks it (localfs has it; env-file/openbao do not). This is the
// capability-composition gate: wiring files onto a non-versioned store fails
// fast at construction rather than silently losing compare-and-set semantics.
func New(s store.Store) (*Object, error) {
	v, ok := s.(store.Versioned)
	if !ok {
		return nil, ErrNotVersioned
	}
	return &Object{store: s, versioned: v}, nil
}

// Read returns the raw UTF-8 text and etag for relpath. It enforces the size
// cap and UTF-8 validity, and maps store containment failures to
// ErrNotContained.
func (o *Object) Read(ctx context.Context, relpath string) (Content, error) {
	if relpath == "" || relpath == "." {
		return Content{}, ErrNotContained
	}
	blob, ok, err := o.store.Get(ctx, relpath)
	if err != nil {
		return Content{}, mapStoreErr(err)
	}
	if !ok {
		return Content{}, ErrNotFound
	}
	if len(blob.Bytes) > MaxFileBytes {
		return Content{}, ErrTooLarge
	}
	if !utf8.Valid(blob.Bytes) {
		return Content{}, ErrNotText
	}
	return Content{Path: relpath, Content: string(blob.Bytes), Etag: blob.Etag}, nil
}

// Write stores content at relpath. A non-empty etag is a compare-and-set
// precondition; a mismatch returns ErrEtagMismatch. Content must be valid
// UTF-8 within the size cap.
func (o *Object) Write(ctx context.Context, relpath, content, etag string) (Ref, error) {
	if relpath == "" || relpath == "." {
		return Ref{}, ErrNotContained
	}
	if len(content) > MaxFileBytes {
		return Ref{}, ErrTooLarge
	}
	if !utf8.ValidString(content) {
		return Ref{}, ErrNotText
	}
	blob, err := o.versioned.SetIf(ctx, relpath, store.Blob{Bytes: []byte(content)}, etag)
	if err != nil {
		return Ref{}, mapStoreErr(err)
	}
	return Ref{Path: relpath, Etag: blob.Etag}, nil
}

// mapStoreErr translates store sentinels into files sentinels.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrEtagMismatch):
		return ErrEtagMismatch
	case errors.Is(err, store.ErrNotContained):
		return ErrNotContained
	case errors.Is(err, store.ErrTooLarge):
		return ErrTooLarge
	default:
		return err
	}
}
