package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A failed command keeps its historical text, and its output separately.
func TestRunReturnsCommandErrorWithOutput(t *testing.T) {
	_, err := New().Run(t.Context(), t.TempDir(), "rev-parse", "--verify", "no-such-ref")
	var command *CommandError
	if !errors.As(err, &command) {
		t.Fatalf("Run() error = %v (%T), want *CommandError", err, err)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Run() error does not unwrap to *exec.ExitError")
	}
	if !strings.HasPrefix(err.Error(), "git [rev-parse --verify no-such-ref]: exit status ") || !strings.Contains(command.Output, "fatal:") ||
		!strings.HasSuffix(err.Error(), command.Output) {
		t.Fatalf("Run() error = %q, output %q; want the git [args]: status: output form", err.Error(), command.Output)
	}
}

// DirtyPaths lists modified, staged, untracked and renamed paths, including
// ones git would quote.
func TestDirtyPaths(t *testing.T) {
	isolateGitConfig(t)
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	mustWriteFile(t, filepath.Join(repo, "tracked.txt"), "one\n")
	mustWriteFile(t, filepath.Join(repo, "old name.txt"), "rename me\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "-m", "initial")

	client := New()
	if paths, err := client.DirtyPaths(t.Context(), repo); err != nil || len(paths) != 0 {
		t.Fatalf("DirtyPaths(clean) = %v, %v; want none", paths, err)
	}
	mustWriteFile(t, filepath.Join(repo, "tracked.txt"), "two\n")
	runGit(t, repo, "mv", "old name.txt", "new name.txt")
	if err := os.MkdirAll(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}
	mustWriteFile(t, filepath.Join(repo, "dir", "quote\"d.txt"), "new\n")
	paths, err := client.DirtyPaths(t.Context(), repo)
	if err != nil {
		t.Fatalf("DirtyPaths() error = %v", err)
	}
	slices.Sort(paths)
	if want := []string{"dir/quote\"d.txt", "new name.txt", "tracked.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("DirtyPaths() = %q, want %q", paths, want)
	}
}

// Output that echoes a credential-bearing URL is redacted where git ran, so
// the CommandError never holds the credential.
func TestRunRedactsCommandOutput(t *testing.T) {
	_, err := Client{Bin: "sh"}.Run(t.Context(), "", "-c", "echo 'fatal: https://user:s3cret@example.com/x.git'; exit 1")
	var command *CommandError
	if !errors.As(err, &command) {
		t.Fatalf("Run() error = %v, want *CommandError", err)
	}
	if strings.Contains(command.Output, "s3cret") || strings.Contains(err.Error(), "s3cret") || !strings.Contains(command.Output, "https://***@example.com/x.git") {
		t.Fatalf("output = %q, want the credential masked", command.Output)
	}
}
