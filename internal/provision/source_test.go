package provision

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/internal/config"
)

func TestMaterializeSourcesCopiesLocalTree(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "file.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "ignored"), []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"app": {Kind: "local", Path: "source", Target: "code"},
	}, true)
	if err != nil {
		t.Fatalf("MaterializeSources() error: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %#v", changed)
	}
	data, err := os.ReadFile(filepath.Join(base, "code", "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("copied data = %q", data)
	}
	if _, err := os.Stat(filepath.Join(base, "code", ".git", "ignored")); !os.IsNotExist(err) {
		t.Fatalf("expected .git directory to be skipped, got %v", err)
	}
}

func TestMaterializeSourcesRejectsEscapingTarget(t *testing.T) {
	_, err := MaterializeSources(context.Background(), t.TempDir(), map[string]config.SourceSpec{
		"app": {Kind: "local", Target: "../escape"},
	}, true)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected escaping target error, got %v", err)
	}
}

func TestMaterializeSourcesDownloadsURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downloaded"))
	}))
	defer server.Close()
	base := t.TempDir()
	changed, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"asset": {Kind: "url", URL: server.URL + "/asset.txt", Target: "assets"},
	}, true)
	if err != nil {
		t.Fatalf("MaterializeSources() error: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %#v", changed)
	}
	data, err := os.ReadFile(filepath.Join(base, "assets", "asset.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "downloaded" {
		t.Fatalf("downloaded data = %q", data)
	}
}

func TestMaterializeSourcesExtractsZipArchive(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "source.zip")
	writeZip(t, archivePath, map[string]string{"nested/file.txt": "archived"})
	changed, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"archive": {Kind: "archive", Path: "source.zip", Target: "code"},
	}, true)
	if err != nil {
		t.Fatalf("MaterializeSources() error: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %#v", changed)
	}
	data, err := os.ReadFile(filepath.Join(base, "code", "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "archived" {
		t.Fatalf("extracted data = %q", data)
	}
}

func TestMaterializeSourcesRejectsUnsafeZipArchive(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "unsafe.zip")
	writeZip(t, archivePath, map[string]string{"../escape.txt": "bad"})
	_, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"archive": {Kind: "archive", Path: "unsafe.zip", Target: "code"},
	}, true)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected unsafe archive error, got %v", err)
	}
}

func TestMaterializeSourcesClonesGitRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	initGitSourceRepo(t, repo)

	changed, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"app": {Kind: "git", Repo: repo, Ref: "main", Target: "code"},
	}, true)
	if err != nil {
		t.Fatalf("MaterializeSources() error: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %#v", changed)
	}
	data, err := os.ReadFile(filepath.Join(base, "code", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from git\n" {
		t.Fatalf("cloned data = %q", data)
	}
}

func TestMaterializeSourcesSyncsExistingGitRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	initGitSourceRepo(t, repo)
	if _, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"app": {Kind: "git", Repo: repo, Ref: "main", Target: "code"},
	}, true); err != nil {
		t.Fatalf("initial MaterializeSources() error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("updated\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-m", "update")

	changed, err := MaterializeSources(context.Background(), base, map[string]config.SourceSpec{
		"app": {Kind: "git", Repo: repo, Ref: "main", Target: "code"},
	}, true)
	if err != nil {
		t.Fatalf("sync MaterializeSources() error: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %#v", changed)
	}
	data, err := os.ReadFile(filepath.Join(base, "code", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "updated\n" {
		t.Fatalf("synced data = %q", data)
	}
}

func initGitSourceRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, repo, "init", "-b", "main")
	runGitIn(t, repo, "config", "user.name", "angee-test")
	runGitIn(t, repo, "config", "user.email", "angee-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("from git\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, repo, "add", ".")
	runGitIn(t, repo, "commit", "-m", "initial")
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(out)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			_ = writer.Close()
			_ = out.Close()
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			_ = writer.Close()
			_ = out.Close()
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
