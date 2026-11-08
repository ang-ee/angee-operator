// Package toolinstall resolves optional Go-installed command-line tools and,
// when an interactive caller allows it, offers to install a missing tool.
package toolinstall

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

// Tool describes a binary installed with `go install`.
type Tool struct {
	Name           string
	InstallPackage string
}

// ResolveOptions controls one binary lookup and optional install prompt.
// The function fields are test seams; nil selects the real implementation.
type ResolveOptions struct {
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	Prompt        bool
	InputExplicit bool
	LookupPath    func(string) (string, error)
	GoBinPath     func(context.Context) (string, error)
	Install       func(context.Context, io.Writer, io.Writer) error
}

// Resolve finds tool on PATH, then in $(go env GOPATH)/bin. If both lookups
// fail, it offers the pinned go install command only when Prompt is true and
// Stdin is a terminal (or an explicit injected reader). A successful install
// is resolved again before returning.
func (tool Tool) Resolve(ctx context.Context, options ResolveOptions) (string, error) {
	lookup := options.LookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	goBinPath := options.GoBinPath
	if goBinPath == nil {
		goBinPath = defaultGoBinPath
	}
	install := options.Install
	if install == nil {
		install = tool.installer
	}

	find := func() (string, error) {
		if path, err := lookup(tool.Name); err == nil {
			return path, nil
		}
		goBin, err := goBinPath(ctx)
		if err != nil {
			return "", err
		}
		name := tool.Name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(goBin, name)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", path)
		}
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}

	if path, err := find(); err == nil {
		return path, nil
	}
	if !options.Prompt || !canPrompt(options.Stdin, options.InputExplicit) {
		return "", tool.missingError()
	}
	if !tool.confirm(options.Stdin, options.Stderr) {
		return "", tool.missingError()
	}
	if err := install(ctx, options.Stdout, options.Stderr); err != nil {
		return "", fmt.Errorf("install %s: %w", tool.Name, err)
	}
	if path, err := find(); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s was installed but is not executable; add $(go env GOPATH)/bin to PATH", tool.Name)
}

func (tool Tool) missingError() error {
	return fmt.Errorf("%s is required; install it with `go install %s` or add it to PATH", tool.Name, tool.InstallPackage)
}

// defaultGoBinPath returns the binary directory below `go env GOPATH`.
func defaultGoBinPath(ctx context.Context) (string, error) {
	args := []string{"env", "GOPATH"}
	trace := logctx.TraceExec(ctx, "go", args, "")
	out, err := exec.CommandContext(ctx, "go", args...).Output()
	trace(out, err)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("GOPATH is empty")
	}
	return filepath.Join(path, "bin"), nil
}

// canPrompt reports whether stdin is suitable for an install confirmation.
// An explicitly injected reader is considered interactive so tests and other
// already-interactive frontends can provide their own input stream.
func canPrompt(stdin io.Reader, explicit bool) bool {
	if stdin == nil {
		return false
	}
	if explicit {
		return true
	}
	file, ok := stdin.(*os.File)
	if !ok {
		return true
	}
	stat, err := file.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

func (tool Tool) confirm(stdin io.Reader, stderr io.Writer) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	_, _ = fmt.Fprintf(stderr, "%s is required but was not found. Install it now with `go install %s`? [y/N] ", tool.Name, tool.InstallPackage)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && len(line) == 0 {
		_, _ = fmt.Fprintln(stderr)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func (tool Tool) installer(ctx context.Context, stdout io.Writer, stderr io.Writer) error {
	args := []string{"install", tool.InstallPackage}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	trace := logctx.TraceExec(ctx, "go", args, "", slog.String("tool", tool.Name))
	err := cmd.Run()
	trace(nil, err)
	return err
}
