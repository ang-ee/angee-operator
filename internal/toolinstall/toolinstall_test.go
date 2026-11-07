package toolinstall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveSearchesPathThenGoBin(t *testing.T) {
	tool := Tool{Name: "sample", InstallPackage: "example.invalid/sample@v1.2.3"}
	name := "sample"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	want := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(want, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := tool.Resolve(t.Context(), ResolveOptions{
		LookupPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		GoBinPath:  func(context.Context) (string, error) { return filepath.Dir(want), nil },
	})
	if err != nil || path != want {
		t.Fatalf("Resolve() = %q, %v; want %q", path, err, want)
	}
}

func TestResolveRejectsNonExecutableGoBinFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows executable discovery uses file extensions rather than Unix mode bits")
	}
	tool := Tool{Name: "sample", InstallPackage: "example.invalid/sample@v1.2.3"}
	goBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(goBin, "sample"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Resolve(t.Context(), ResolveOptions{
		LookupPath: func(string) (string, error) { return "", errors.New("not on PATH") },
		GoBinPath:  func(context.Context) (string, error) { return goBin, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "go install example.invalid/sample@v1.2.3") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolvePromptsInstallsAndResolvesAgain(t *testing.T) {
	tool := Tool{Name: "sample", InstallPackage: "example.invalid/sample@v1.2.3"}
	installed := false
	var stderr bytes.Buffer
	path, err := tool.Resolve(t.Context(), ResolveOptions{
		Stdin:         strings.NewReader("yes\n"),
		Stderr:        &stderr,
		Prompt:        true,
		InputExplicit: true,
		LookupPath: func(string) (string, error) {
			if installed {
				return "/tmp/sample", nil
			}
			return "", errors.New("not found")
		},
		GoBinPath: func(context.Context) (string, error) { return "", errors.New("no GOPATH") },
		Install: func(context.Context, io.Writer, io.Writer) error {
			installed = true
			return nil
		},
	})
	if err != nil || path != "/tmp/sample" {
		t.Fatalf("Resolve() = %q, %v", path, err)
	}
	if !strings.Contains(stderr.String(), "go install example.invalid/sample@v1.2.3") {
		t.Fatalf("prompt = %q", stderr.String())
	}
}

func TestResolveDoesNotPromptWithoutPermission(t *testing.T) {
	tool := Tool{Name: "sample", InstallPackage: "example.invalid/sample@v1.2.3"}
	installed := false
	_, err := tool.Resolve(t.Context(), ResolveOptions{
		Stdin: strings.NewReader("yes\n"),
		LookupPath: func(string) (string, error) {
			return "", errors.New("not found")
		},
		GoBinPath: func(context.Context) (string, error) { return "", errors.New("no GOPATH") },
		Install: func(context.Context, io.Writer, io.Writer) error {
			installed = true
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "go install example.invalid/sample@v1.2.3") {
		t.Fatalf("Resolve() error = %v", err)
	}
	if installed {
		t.Fatal("installer ran without prompt permission")
	}
}
