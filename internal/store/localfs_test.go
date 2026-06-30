package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newTestLocalFS(t *testing.T) Store {
	t.Helper()
	s, err := Open("localfs", Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Open localfs: %v", err)
	}
	return s
}

func TestLocalFSCapabilities(t *testing.T) {
	s := newTestLocalFS(t)
	if _, ok := s.(Lister); !ok {
		t.Error("localfs should implement Lister")
	}
	if _, ok := s.(Deleter); !ok {
		t.Error("localfs should implement Deleter")
	}
	if _, ok := s.(Versioned); !ok {
		t.Error("localfs should implement Versioned")
	}
}

func TestLocalFSRoundtrip(t *testing.T) {
	s := newTestLocalFS(t)
	ctx := context.Background()

	if err := s.Set(ctx, "dir/file.txt", Blob{Bytes: []byte("hello")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(ctx, "dir/file.txt")
	if err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if string(got.Bytes) != "hello" {
		t.Fatalf("Get bytes = %q, want %q", got.Bytes, "hello")
	}
	if got.Etag == "" {
		t.Fatal("expected non-empty etag")
	}
	// Stable etag: sha256 of "hello".
	const wantEtag = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got.Etag != wantEtag {
		t.Fatalf("etag = %q, want %q", got.Etag, wantEtag)
	}

	// Missing key.
	_, ok, err = s.Get(ctx, "nope.txt")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for missing key")
	}
}

func TestLocalFSSetIfCAS(t *testing.T) {
	s := newTestLocalFS(t)
	v := s.(Versioned)
	ctx := context.Background()

	// Empty etag: unconditional create.
	b, err := v.SetIf(ctx, "f.txt", Blob{Bytes: []byte("v1")}, "")
	if err != nil {
		t.Fatalf("SetIf create: %v", err)
	}
	if b.Etag == "" {
		t.Fatal("expected etag from SetIf")
	}

	// Matching etag: succeeds and returns new etag.
	b2, err := v.SetIf(ctx, "f.txt", Blob{Bytes: []byte("v2")}, b.Etag)
	if err != nil {
		t.Fatalf("SetIf matching: %v", err)
	}
	if b2.Etag == b.Etag {
		t.Fatal("expected updated etag after content change")
	}

	// Stale etag: conflict.
	_, err = v.SetIf(ctx, "f.txt", Blob{Bytes: []byte("v3")}, b.Etag)
	if !errors.Is(err, ErrEtagMismatch) {
		t.Fatalf("SetIf stale etag err = %v, want ErrEtagMismatch", err)
	}

	// Non-empty etag against absent file: conflict.
	_, err = v.SetIf(ctx, "absent.txt", Blob{Bytes: []byte("x")}, "deadbeef")
	if !errors.Is(err, ErrEtagMismatch) {
		t.Fatalf("SetIf absent err = %v, want ErrEtagMismatch", err)
	}
}

func TestLocalFSContainmentRejected(t *testing.T) {
	s := newTestLocalFS(t)
	v := s.(Versioned)
	ctx := context.Background()

	for _, key := range []string{"../escape", "a/../../escape", "/etc/passwd"} {
		if _, _, err := s.Get(ctx, key); !errors.Is(err, ErrNotContained) {
			t.Errorf("Get(%q) err = %v, want ErrNotContained", key, err)
		}
		if err := s.Set(ctx, key, Blob{Bytes: []byte("x")}); !errors.Is(err, ErrNotContained) {
			t.Errorf("Set(%q) err = %v, want ErrNotContained", key, err)
		}
		if _, err := v.SetIf(ctx, key, Blob{Bytes: []byte("x")}, ""); !errors.Is(err, ErrNotContained) {
			t.Errorf("SetIf(%q) err = %v, want ErrNotContained", key, err)
		}
	}
}

func TestLocalFSSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not run on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside root pointing outside.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	s, err := Open("localfs", Config{Root: root})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if _, _, err := s.Get(ctx, "link/secret"); !errors.Is(err, ErrNotContained) {
		t.Errorf("Get through escaping symlink err = %v, want ErrNotContained", err)
	}
	if err := s.Set(ctx, "link/new.txt", Blob{Bytes: []byte("x")}); !errors.Is(err, ErrNotContained) {
		t.Errorf("Set through escaping symlink err = %v, want ErrNotContained", err)
	}
}

func TestLocalFSSymlinkInsideOK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test not run on windows")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	s, err := Open("localfs", Config{Root: root})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if err := s.Set(ctx, "link/inside.txt", Blob{Bytes: []byte("ok")}); err != nil {
		t.Fatalf("Set through in-root symlink: %v", err)
	}
	got, ok, err := s.Get(ctx, "link/inside.txt")
	if err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if string(got.Bytes) != "ok" {
		t.Fatalf("Get = %q, want %q", got.Bytes, "ok")
	}
}

func TestLocalFSListAndDelete(t *testing.T) {
	s := newTestLocalFS(t)
	ctx := context.Background()
	_ = s.Set(ctx, "a.txt", Blob{Bytes: []byte("1")})
	_ = s.Set(ctx, "sub/b.txt", Blob{Bytes: []byte("2")})

	keys, err := s.(Lister).List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 || keys[0] != "a.txt" || keys[1] != "sub/b.txt" {
		t.Fatalf("List = %v, want [a.txt sub/b.txt]", keys)
	}

	if err := s.(Deleter).Delete(ctx, "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "a.txt"); ok {
		t.Fatal("expected a.txt gone after Delete")
	}
}

func TestLocalFSMaxBytesRejectsBeforeRead(t *testing.T) {
	root := t.TempDir()
	// Seed an oversized file directly on disk.
	if err := os.WriteFile(filepath.Join(root, "big.txt"), make([]byte, 32), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Open("localfs", Config{Root: root, MaxBytes: 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if _, _, err := s.Get(ctx, "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Get oversized: want ErrTooLarge, got %v", err)
	}
	// A within-cap file still reads fine.
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("small"), 0o644); err != nil {
		t.Fatalf("seed ok: %v", err)
	}
	if _, ok, err := s.Get(ctx, "ok.txt"); err != nil || !ok {
		t.Fatalf("Get within cap: ok=%v err=%v", ok, err)
	}
}

func TestLocalFSSetIfConcurrentNoLostUpdate(t *testing.T) {
	root := t.TempDir()
	s, err := Open("localfs", Config{Root: root})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	v := s.(Versioned)
	ctx := context.Background()

	base, err := v.SetIf(ctx, "f.txt", Blob{Bytes: []byte("0")}, "")
	if err != nil {
		t.Fatalf("seed SetIf: %v", err)
	}

	// Two writers race on the same base etag; exactly one must win, the other
	// must observe ErrEtagMismatch (no silent clobber).
	type res struct {
		err error
	}
	ch := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := v.SetIf(ctx, "f.txt", Blob{Bytes: []byte("x")}, base.Etag)
			ch <- res{err}
		}()
	}
	var wins, conflicts int
	for i := 0; i < 2; i++ {
		r := <-ch
		switch {
		case r.err == nil:
			wins++
		case errors.Is(r.err, ErrEtagMismatch):
			conflicts++
		default:
			t.Fatalf("unexpected SetIf error: %v", r.err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("want exactly 1 win + 1 conflict, got wins=%d conflicts=%d", wins, conflicts)
	}
}
