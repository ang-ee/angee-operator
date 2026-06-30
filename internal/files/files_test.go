package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/store"
)

// nonVersionedStore implements store.Store but deliberately NOT store.Versioned,
// to exercise the capability gate in New.
type nonVersionedStore struct{}

func (nonVersionedStore) Get(context.Context, string) (store.Blob, bool, error) {
	return store.Blob{}, false, nil
}
func (nonVersionedStore) Set(context.Context, string, store.Blob) error { return nil }

// newTestObject builds a localfs-backed object rooted in a temp dir. maxBytes is
// the store-level cap (0 = unlimited, so the object layer's own caps are
// exercised in isolation).
func newTestObject(t *testing.T, maxBytes int64) (*Object, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open("localfs", store.Config{Root: root, MaxBytes: maxBytes})
	if err != nil {
		t.Fatalf("open localfs: %v", err)
	}
	obj, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return obj, root
}

func TestNewRequiresVersioned(t *testing.T) {
	if _, err := New(nonVersionedStore{}); !errors.Is(err, ErrNotVersioned) {
		t.Fatalf("New(non-versioned) err = %v, want ErrNotVersioned", err)
	}
	// localfs has the Versioned capability, so New succeeds.
	s, err := store.Open("localfs", store.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("open localfs: %v", err)
	}
	if _, err := New(s); err != nil {
		t.Fatalf("New(localfs) err = %v, want nil", err)
	}
}

func TestReadWriteRoundtrip(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	ctx := context.Background()

	ref, err := obj.Write(ctx, "dir/app.yaml", "hello: world\n", "")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ref.Path != "dir/app.yaml" || ref.Etag == "" {
		t.Fatalf("Write ref = %+v", ref)
	}

	got, err := obj.Read(ctx, "dir/app.yaml")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Content != "hello: world\n" {
		t.Fatalf("content = %q, want %q", got.Content, "hello: world\n")
	}
	if got.Etag != ref.Etag {
		t.Fatalf("read etag %q != write etag %q", got.Etag, ref.Etag)
	}
}

func TestReadNotFound(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	if _, err := obj.Read(context.Background(), "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read missing err = %v, want ErrNotFound", err)
	}
}

func TestWriteEtagPrecondition(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	ctx := context.Background()

	ref, err := obj.Write(ctx, "f.txt", "v1", "")
	if err != nil {
		t.Fatalf("Write create: %v", err)
	}
	// A matching etag updates and rolls the etag forward.
	ref2, err := obj.Write(ctx, "f.txt", "v2", ref.Etag)
	if err != nil {
		t.Fatalf("Write matching etag: %v", err)
	}
	if ref2.Etag == ref.Etag {
		t.Fatal("expected updated etag after content change")
	}
	// The now-stale original etag conflicts.
	if _, err := obj.Write(ctx, "f.txt", "v3", ref.Etag); !errors.Is(err, ErrEtagMismatch) {
		t.Fatalf("Write stale etag err = %v, want ErrEtagMismatch", err)
	}
}

func TestWriteRejectsNonUTF8(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	bad := string([]byte{0xff, 0xfe, 0xfd})
	if _, err := obj.Write(context.Background(), "f.bin", bad, ""); !errors.Is(err, ErrNotText) {
		t.Fatalf("Write non-utf8 err = %v, want ErrNotText", err)
	}
}

func TestWriteRejectsOversize(t *testing.T) {
	obj, root := newTestObject(t, 0)
	big := strings.Repeat("a", MaxFileBytes+1)
	if _, err := obj.Write(context.Background(), "big.txt", big, ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Write oversize err = %v, want ErrTooLarge", err)
	}
	// The oversized content must be rejected before any bytes hit disk.
	if _, err := os.Stat(filepath.Join(root, "big.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize Write should not have created the file (stat err = %v)", err)
	}
}

func TestReadRejectsOversizeOnDisk(t *testing.T) {
	// Seed a file larger than the object cap directly on disk, with the store cap
	// disabled, so the object layer's own Read cap is the thing under test.
	obj, root := newTestObject(t, 0)
	big := []byte(strings.Repeat("a", MaxFileBytes+1))
	if err := os.WriteFile(filepath.Join(root, "big.txt"), big, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := obj.Read(context.Background(), "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read oversize err = %v, want ErrTooLarge", err)
	}
}

func TestReadRejectsNonUTF8OnDisk(t *testing.T) {
	obj, root := newTestObject(t, 0)
	if err := os.WriteFile(filepath.Join(root, "f.bin"), []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := obj.Read(context.Background(), "f.bin"); !errors.Is(err, ErrNotText) {
		t.Fatalf("Read non-utf8 err = %v, want ErrNotText", err)
	}
}

func TestContainmentMappedToNotContained(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	ctx := context.Background()
	for _, key := range []string{"../escape.txt", "a/../../escape.txt", "/etc/passwd"} {
		if _, err := obj.Read(ctx, key); !errors.Is(err, ErrNotContained) {
			t.Errorf("Read(%q) err = %v, want ErrNotContained", key, err)
		}
		if _, err := obj.Write(ctx, key, "x", ""); !errors.Is(err, ErrNotContained) {
			t.Errorf("Write(%q) err = %v, want ErrNotContained", key, err)
		}
	}
}

func TestEmptyOrDotRelpathRejected(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	ctx := context.Background()
	for _, key := range []string{"", "."} {
		if _, err := obj.Read(ctx, key); !errors.Is(err, ErrNotContained) {
			t.Errorf("Read(%q) err = %v, want ErrNotContained", key, err)
		}
		if _, err := obj.Write(ctx, key, "x", ""); !errors.Is(err, ErrNotContained) {
			t.Errorf("Write(%q) err = %v, want ErrNotContained", key, err)
		}
	}
}

func TestReadDirectoryIsNotFound(t *testing.T) {
	obj, _ := newTestObject(t, 0)
	ctx := context.Background()
	// Create a file so its parent "dir" exists as a directory.
	if _, err := obj.Write(ctx, "dir/x.txt", "hi", ""); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	// Reading the directory path itself is a clean not-found, not a raw error.
	if _, err := obj.Read(ctx, "dir"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(dir) err = %v, want ErrNotFound", err)
	}
}
