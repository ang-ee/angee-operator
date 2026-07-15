package operator

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

type jobRunResult struct {
	output []byte
	err    error
}

func TestOperatorJobRunStreamsLocalOutputWhileRunning(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "release")
	writeJobOutputTestStack(t, root, "codegen", manifest.Job{
		Runtime: manifest.RuntimeLocal,
		Command: []string{
			"sh",
			"-c",
			`printf 'first\n'; while [ ! -f "$RELEASE" ]; do sleep 0.01; done; printf 'second\n'`,
		},
		Env: map[string]string{"RELEASE": release},
	})

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, jobOutput: writer})
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		_ = writer.Close()
		_ = reader.Close()
	})

	lines, scanErr := scanJobOutput(reader)
	result := make(chan jobRunResult, 1)
	go func() {
		output, runErr := server.platform.JobRun(context.Background(), "codegen", nil)
		result <- jobRunResult{output: output, err: runErr}
	}()

	seen := waitForJobOutputLine(t, lines, "first")
	select {
	case got := <-result:
		t.Fatalf("JobRun() completed before release: output = %q, error = %v", got.output, got.err)
	default:
	}
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(release) error = %v", err)
	}

	var got jobRunResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("JobRun() did not complete after release")
	}
	if got.err != nil {
		t.Fatalf("JobRun() error = %v", got.err)
	}
	if string(got.output) != "first\nsecond\n" {
		t.Fatalf("JobRun() output = %q, want %q", got.output, "first\nsecond\n")
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Close(job output writer) error = %v", err)
	}
	for line := range lines {
		seen = append(seen, line)
	}
	if err := <-scanErr; err != nil {
		t.Fatalf("scan job output error = %v", err)
	}
	terminal := strings.Join(seen, "\n")
	for _, want := range []string{
		"[job codegen] running",
		"first",
		"second",
		"[job codegen] finished",
	} {
		if !strings.Contains(terminal, want) {
			t.Fatalf("terminal output = %q, want %q", terminal, want)
		}
	}
	if strings.Count(terminal, "first") != 1 || strings.Count(terminal, "second") != 1 {
		t.Fatalf("terminal output duplicated command output: %q", terminal)
	}
}

func TestOperatorJobRunStreamsPartialOutputOnFailure(t *testing.T) {
	root := t.TempDir()
	writeJobOutputTestStack(t, root, "broken", manifest.Job{
		Runtime: manifest.RuntimeLocal,
		Command: []string{"sh", "-c", `printf 'partial\n'; exit 7`},
	})

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, jobOutput: writer})
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})

	output, runErr := server.platform.JobRun(context.Background(), "broken", nil)
	if runErr == nil {
		t.Fatal("JobRun() error = nil, want failed command error")
	}
	if string(output) != "partial\n" {
		t.Fatalf("JobRun() output = %q, want %q", output, "partial\n")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(job output writer) error = %v", err)
	}
	terminalOutput, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll(job output) error = %v", err)
	}
	terminal := string(terminalOutput)
	for _, want := range []string{
		"[job broken] running\n",
		"partial\n",
		"[job broken] failed\n",
	} {
		if !strings.Contains(terminal, want) {
			t.Fatalf("terminal output = %q, want %q", terminal, want)
		}
	}
	if strings.Contains(terminal, "[job broken] finished") {
		t.Fatalf("terminal output = %q, failed job must not be marked finished", terminal)
	}
}

func TestOperatorJobRunStreamsContainerOutput(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "container-release")
	writeJobOutputTestStack(t, root, "codegen-container", manifest.Job{
		Runtime: manifest.RuntimeContainer,
		Image:   "example/codegen:test",
		Command: []string{"generate"},
	})

	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	fakeDocker := `#!/bin/sh
printf 'container-stderr\n' >&2
while [ ! -f "$FAKE_DOCKER_RELEASE" ]; do sleep 0.01; done
printf 'container-stdout\n'
`
	if err := os.WriteFile(dockerPath, []byte(fakeDocker), 0o755); err != nil {
		t.Fatalf("WriteFile(fake docker) error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_RELEASE", release)

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, jobOutput: writer})
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release\n"), 0o600)
		_ = writer.Close()
		_ = reader.Close()
	})

	lines, scanErr := scanJobOutput(reader)
	result := make(chan jobRunResult, 1)
	go func() {
		output, runErr := server.platform.JobRun(context.Background(), "codegen-container", nil)
		result <- jobRunResult{output: output, err: runErr}
	}()

	seen := waitForJobOutputLine(t, lines, "container-stderr")
	select {
	case got := <-result:
		t.Fatalf("JobRun() completed before release: output = %q, error = %v", got.output, got.err)
	default:
	}
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(release) error = %v", err)
	}

	var got jobRunResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("JobRun() did not complete after release")
	}
	if got.err != nil {
		t.Fatalf("JobRun() error = %v", got.err)
	}
	if string(got.output) != "container-stderr\ncontainer-stdout\n" {
		t.Fatalf("JobRun() output = %q, want %q", got.output, "container-stderr\ncontainer-stdout\n")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(job output writer) error = %v", err)
	}
	for line := range lines {
		seen = append(seen, line)
	}
	if err := <-scanErr; err != nil {
		t.Fatalf("scan job output error = %v", err)
	}
	terminal := strings.Join(seen, "\n")
	for _, want := range []string{
		"[job codegen-container] running",
		"container-stderr",
		"container-stdout",
		"[job codegen-container] finished",
	} {
		if !strings.Contains(terminal, want) {
			t.Fatalf("terminal output = %q, want %q", terminal, want)
		}
	}
	if strings.Count(terminal, "container-stderr") != 1 || strings.Count(terminal, "container-stdout") != 1 {
		t.Fatalf("terminal output duplicated command output: %q", terminal)
	}
}

func TestOperatorJobRunIgnoresTerminalWriterErrors(t *testing.T) {
	root := t.TempDir()
	writeJobOutputTestStack(t, root, "codegen", manifest.Job{
		Runtime: manifest.RuntimeLocal,
		Command: []string{"sh", "-c", `printf 'generated\n'`},
	})
	server, err := NewServer(Config{
		Root:      root,
		Bind:      "127.0.0.1",
		Port:      9000,
		jobOutput: failingJobOutputWriter{},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	output, runErr := server.platform.JobRun(context.Background(), "codegen", nil)
	if runErr != nil {
		t.Fatalf("JobRun() error = %v, terminal writer errors must be best effort", runErr)
	}
	if string(output) != "generated\n" {
		t.Fatalf("JobRun() output = %q, want %q", output, "generated\n")
	}
}

type failingJobOutputWriter struct{}

func (failingJobOutputWriter) Write([]byte) (int, error) {
	return 0, errors.New("terminal unavailable")
}

func writeJobOutputTestStack(t *testing.T, root, name string, job manifest.Job) {
	t.Helper()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "job-output-test",
		Jobs:    map[string]manifest.Job{name: job},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
}

func scanJobOutput(reader *os.File) (<-chan string, <-chan error) {
	lines := make(chan string, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		errs <- scanner.Err()
	}()
	return lines, errs
}

func waitForJobOutputLine(t *testing.T, lines <-chan string, want string) []string {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var seen []string
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("job output closed before %q; saw %q", want, seen)
			}
			seen = append(seen, line)
			if strings.Contains(line, want) {
				return seen
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for job output %q; saw %q", want, seen)
		}
	}
}
