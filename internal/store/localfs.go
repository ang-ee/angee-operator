package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func init() {
	Register("localfs", newLocalFS)
}

// localFS is a key->file store rooted at a single directory. Each key is a
// relative path mapped to one file on disk; the etag is the sha256 hex of the
// file bytes. It implements Store, Lister, Deleter, and Versioned.
type localFS struct {
	root     string // pre-cleaned absolute base dir
	maxBytes int64  // per-file size cap; 0 means unlimited
}

func newLocalFS(cfg Config) (Store, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("localfs: Root is required")
	}
	abs, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("localfs: resolve root: %w", err)
	}
	// Canonicalize the root through any symlinks (e.g. macOS /var ->
	// /private/var) so later containment checks compare like with like.
	root, err := evalExisting(filepath.Clean(abs))
	if err != nil {
		return nil, fmt.Errorf("localfs: resolve root: %w", err)
	}
	return &localFS{root: root, maxBytes: cfg.MaxBytes}, nil
}

// rootMu serializes the compare-and-set read-modify-write in SetIf per resolved
// root. localFS instances are constructed fresh per request (store.Open), so the
// mutex must live at package scope keyed by the canonical root path; an
// instance-level lock would not serialize two concurrent operator requests that
// each opened their own localFS over the same source dir. Combined with the
// atomic temp+rename in writeFile, this closes the lost-update CAS race and the
// torn-read window.
var (
	rootMuOnce sync.Mutex
	rootMu     = map[string]*sync.Mutex{}
)

func lockForRoot(root string) *sync.Mutex {
	rootMuOnce.Lock()
	defer rootMuOnce.Unlock()
	m, ok := rootMu[root]
	if !ok {
		m = &sync.Mutex{}
		rootMu[root] = m
	}
	return m
}

// statInfo returns the FileInfo for abs, or (nil,false,nil) when it does not
// exist. It lets callers enforce maxBytes BEFORE allocating a read buffer and
// distinguish a directory from a regular file.
func (l *localFS) statInfo(abs string) (os.FileInfo, bool, error) {
	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return info, true, nil
}

var (
	_ Store     = (*localFS)(nil)
	_ Lister    = (*localFS)(nil)
	_ Deleter   = (*localFS)(nil)
	_ Versioned = (*localFS)(nil)
)

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (l *localFS) Get(ctx context.Context, key string) (Blob, bool, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, false, err
	}
	abs, err := l.resolve(key)
	if err != nil {
		return Blob{}, false, err
	}
	// Enforce the size cap before reading so an oversized file never gets
	// slurped into memory (DoS guard); os.ReadFile would otherwise allocate the
	// full file before any caller-side cap could reject it.
	info, ok, err := l.statInfo(abs)
	if err != nil {
		return Blob{}, false, err
	}
	if !ok || info.IsDir() {
		// A missing path or a directory is not a readable file; report absent so
		// the object layer maps it to a clean not-found rather than a raw EISDIR.
		return Blob{}, false, nil
	}
	if l.maxBytes > 0 && info.Size() > l.maxBytes {
		return Blob{}, false, ErrTooLarge
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Blob{}, false, nil
		}
		return Blob{}, false, err
	}
	return Blob{Bytes: b, Etag: etagOf(b)}, true, nil
}

func (l *localFS) Set(ctx context.Context, key string, value Blob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := l.resolve(key)
	if err != nil {
		return err
	}
	return writeFile(abs, value.Bytes)
}

func (l *localFS) SetIf(ctx context.Context, key string, value Blob, expectedEtag string) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	abs, err := l.resolve(key)
	if err != nil {
		return Blob{}, err
	}
	// Serialize the read-compare-write so two writers that fetched the same base
	// etag cannot both pass the check and clobber each other (lost update).
	mu := lockForRoot(l.root)
	mu.Lock()
	defer mu.Unlock()
	if expectedEtag != "" {
		info, ok, err := l.statInfo(abs)
		if err != nil {
			return Blob{}, err
		}
		if !ok || info.IsDir() {
			// A non-empty expectedEtag asserts the file exists with that
			// version; an absent path or a directory cannot satisfy it.
			return Blob{}, ErrEtagMismatch
		}
		if l.maxBytes > 0 && info.Size() > l.maxBytes {
			return Blob{}, ErrTooLarge
		}
		current, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Blob{}, ErrEtagMismatch
			}
			return Blob{}, err
		}
		if etagOf(current) != expectedEtag {
			return Blob{}, ErrEtagMismatch
		}
	}
	if err := writeFile(abs, value.Bytes); err != nil {
		return Blob{}, err
	}
	return Blob{Bytes: value.Bytes, Etag: etagOf(value.Bytes)}, nil
}

func (l *localFS) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *localFS) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var keys []string
	err := filepath.WalkDir(l.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// writeFile writes b to abs atomically: it stages the bytes in a temp file in
// the same directory (so the rename stays on one filesystem) and renames it into
// place. os.Rename is atomic on a single filesystem, so a concurrent Get sees
// either the old or the new complete file — never the torn, partially-written
// state os.WriteFile's in-place truncate would expose.
func writeFile(abs string, b []byte) error {
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(abs)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, abs)
}

// resolve maps a relative key to an absolute path strictly contained within
// root. This is THE single contained resolver for the store layer; do not
// reintroduce ad hoc containment checks elsewhere.
func (l *localFS) resolve(key string) (string, error) {
	if filepath.IsAbs(key) {
		return "", ErrNotContained
	}
	clean := filepath.Clean(filepath.FromSlash(key))
	sep := string(os.PathSeparator)
	if clean == ".." || strings.HasPrefix(clean, ".."+sep) {
		return "", ErrNotContained
	}
	abs := filepath.Clean(filepath.Join(l.root, clean))
	// Lexical containment: abs must equal root or sit under root+sep.
	if abs != l.root && !strings.HasPrefix(abs, l.root+sep) {
		return "", ErrNotContained
	}
	// Symlink hardening: resolve symlinks on the deepest existing path and
	// re-verify the result is still inside root. For a not-yet-created leaf,
	// evaluate the nearest existing parent chain instead.
	resolved, err := evalExisting(abs)
	if err != nil {
		return "", err
	}
	if resolved != l.root && !strings.HasPrefix(resolved, l.root+sep) {
		return "", ErrNotContained
	}
	// Operate on the symlink-resolved path (not the lexical abs) so later FS ops
	// can't re-follow a component swapped for an escaping symlink between this
	// check and the operation (TOCTOU hardening).
	return resolved, nil
}

// evalExisting walks up from abs to the first path component that exists,
// resolves symlinks on it, and re-appends the not-yet-existing tail. The
// returned path is cleaned and symlink-free up to the existing prefix.
func evalExisting(abs string) (string, error) {
	tail := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if tail == "" {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, tail)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding an existing prefix.
			return filepath.Clean(abs), nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}
