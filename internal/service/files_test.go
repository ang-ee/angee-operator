package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// setupFilesFixture builds a temp root whose manifest declares a `local`
// source `app` pointing at the `app/` subdir, and creates that subdir.
func setupFilesFixture(t *testing.T) *Platform {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {Kind: "local", Path: "app"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestFileWriteThenReadRoundtrip(t *testing.T) {
	p := setupFilesFixture(t)
	ctx := context.Background()

	ref, err := p.FileWrite(ctx, "app", "config/app.yaml", "hello: world\n", "")
	if err != nil {
		t.Fatalf("FileWrite error = %v", err)
	}
	if ref.Path != "config/app.yaml" || ref.Source != "app" || ref.Etag == "" {
		t.Fatalf("ref = %+v, want path+source+non-empty etag", ref)
	}

	content, err := p.FileRead(ctx, "app", "config/app.yaml")
	if err != nil {
		t.Fatalf("FileRead error = %v", err)
	}
	if content.Content != "hello: world\n" {
		t.Fatalf("content = %q, want %q", content.Content, "hello: world\n")
	}
	if content.Etag != ref.Etag {
		t.Fatalf("read etag %q != write etag %q", content.Etag, ref.Etag)
	}
	if content.Source != "app" || content.Path != "config/app.yaml" {
		t.Fatalf("content meta = %+v", content)
	}

	// A second write with the returned etag succeeds.
	if _, err := p.FileWrite(ctx, "app", "config/app.yaml", "hello: again\n", content.Etag); err != nil {
		t.Fatalf("FileWrite with matching etag error = %v", err)
	}
}

func TestFileWriteEtagMismatchConflict(t *testing.T) {
	p := setupFilesFixture(t)
	ctx := context.Background()

	if _, err := p.FileWrite(ctx, "app", "x.txt", "first", ""); err != nil {
		t.Fatalf("FileWrite error = %v", err)
	}
	_, err := p.FileWrite(ctx, "app", "x.txt", "second", "deadbeef")
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want ConflictError", err)
	}
	if conflict.Kind != "file" {
		t.Fatalf("conflict kind = %q, want file", conflict.Kind)
	}
}

func TestFileReadMissingNotFound(t *testing.T) {
	p := setupFilesFixture(t)
	_, err := p.FileRead(context.Background(), "app", "nope.txt")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
	if notFound.Kind != "file" {
		t.Fatalf("kind = %q, want file", notFound.Kind)
	}
}

func TestFileSourceUnknownNotFound(t *testing.T) {
	p := setupFilesFixture(t)
	_, err := p.FileRead(context.Background(), "missing", "x.txt")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
	if notFound.Kind != "source" {
		t.Fatalf("kind = %q, want source", notFound.Kind)
	}
}

func TestFilePathTraversalRejected(t *testing.T) {
	p := setupFilesFixture(t)
	ctx := context.Background()
	for _, path := range []string{"../escape", "a/../../escape", "/etc/passwd"} {
		_, err := p.FileWrite(ctx, "app", path, "x", "")
		var invalid *InvalidInputError
		if !errors.As(err, &invalid) {
			t.Fatalf("FileWrite(%q) err = %v, want InvalidInputError", path, err)
		}
		if invalid.Field != "path" {
			t.Fatalf("FileWrite(%q) field = %q, want path", path, invalid.Field)
		}
		if _, rerr := p.FileRead(ctx, "app", path); !errors.As(rerr, &invalid) {
			t.Fatalf("FileRead(%q) err = %v, want InvalidInputError", path, rerr)
		}
	}
}

func TestFileWriteOversizeRejected(t *testing.T) {
	p := setupFilesFixture(t)
	big := strings.Repeat("a", (1<<20)+1)
	_, err := p.FileWrite(context.Background(), "app", "big.txt", big, "")
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want InvalidInputError", err)
	}
	if invalid.Field != "content" {
		t.Fatalf("field = %q, want content", invalid.Field)
	}
}

func TestFileWriteBinaryRejected(t *testing.T) {
	p := setupFilesFixture(t)
	// Invalid UTF-8 byte sequence.
	_, err := p.FileWrite(context.Background(), "app", "bin", string([]byte{0xff, 0xfe, 0xfd}), "")
	var invalid *InvalidInputError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want InvalidInputError", err)
	}
	if invalid.Field != "content" {
		t.Fatalf("field = %q, want content", invalid.Field)
	}
}
