package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

func TestDoctorJSONReportsManifestAndMissingSource(t *testing.T) {
	root := t.TempDir()
	writeDoctorManifest(t, root, `version: 1
kind: stack
name: doctor-test
sources:
  app:
    kind: local
    path: missing-app
`)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--root", root, "--json", "doctor"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor JSON did not decode: %v\n%s", err, stdout.String())
	}
	if report.Root != root {
		t.Fatalf("root = %q, want %q", report.Root, root)
	}
	if status := doctorCheckStatus(report, "manifest"); status != doctorOK {
		t.Fatalf("manifest status = %q, want %q", status, doctorOK)
	}
	if status := doctorCheckStatus(report, "source.app"); status != doctorWarn {
		t.Fatalf("source.app status = %q, want %q", status, doctorWarn)
	}
	// Whatever jj this machine has, the check is there and never an error.
	if status := doctorCheckStatus(report, "tool.jj"); status != doctorOK && status != doctorWarn {
		t.Fatalf("tool.jj status = %q, want ok or warn", status)
	}
}

func TestDoctorFailsOnInvalidPortPool(t *testing.T) {
	root := t.TempDir()
	writeDoctorManifest(t, root, `version: 1
kind: stack
name: doctor-test
operator:
  port_pool:
    web:
      range: nope
`)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--root", root, "doctor"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error is nil")
	}
	if !strings.Contains(err.Error(), "doctor found 1 error") {
		t.Fatalf("error = %q, want doctor error count", err)
	}
	if !strings.Contains(stdout.String(), "ERROR port_pool.web") {
		t.Fatalf("doctor output missing port pool error:\n%s", stdout.String())
	}
}

// doctor reports a workspace template that chains a stack as an error, naming
// #110, since workspace create refuses it; an ordinary one stays OK.
func TestDoctorReportsAChainingWorkspaceTemplate(t *testing.T) {
	host := t.TempDir()
	writeTemplate := func(name, copier string) {
		t.Helper()
		dir := filepath.Join(host, ".templates", filepath.FromSlash(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copier), 0o644); err != nil {
			t.Fatalf("WriteFile(%s copier.yml): %v", name, err)
		}
	}
	writeTemplate("stacks/dev", "_angee:\n  kind: stack\n  name: dev\n  chain:\n    - template: ../../projects/web\n      root: .\n")
	writeTemplate("workspaces/dev-pr", "_angee:\n  kind: workspace\n  name: dev-pr\n  chain_root: .angee\n")
	writeTemplate("workspaces/dev-pr-multi", "_angee:\n  kind: workspace\n  name: dev-pr-multi\n")
	t.Chdir(host)

	runner := &doctorRunner{}
	runner.checkTemplates()
	report := doctorReport{Checks: runner.checks}
	for name, want := range map[string]doctorStatus{
		"template.stacks/dev":              doctorOK,
		"template.workspaces/dev-pr":       doctorError,
		"template.workspaces/dev-pr-multi": doctorOK,
	} {
		if got := doctorCheckStatus(report, name); got != want {
			t.Fatalf("%s status = %q, want %q (checks %+v)", name, got, want, runner.checks)
		}
	}
	for _, check := range runner.checks {
		if check.Name == "template.workspaces/dev-pr" && !strings.Contains(check.Detail, "#110") {
			t.Fatalf("chaining template detail = %q, want it to name #110", check.Detail)
		}
	}
}

func TestPickVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		streams []string
		want    string
	}{
		{name: "single line", streams: []string{"git version 2.50.1\n"}, want: "git version 2.50.1"},
		{name: "bare number", streams: []string{"11.21.0\n"}, want: "11.21.0"},
		{
			// process-compose prints a banner before the number and wraps the
			// run in structured debug records; interior tabs are collapsed so
			// the picked line stays inside the report's columns.
			name:    "banner after log noise",
			streams: []string{"{\"level\":\"debug\",\"msg\":\"config home\"}\nProcess Compose\nVersion:\tv1.120.0\nCommit:\t0f3a8e6\n"},
			want:    "Version: v1.120.0",
		},
		{
			// A version number on the second stream beats a bare notice on the
			// first: prefer substance over stream order.
			name:    "version on stderr beats notice on stdout",
			streams: []string{"update available\n", "tool 4.2.0\n"},
			want:    "tool 4.2.0",
		},
		{name: "no version number anywhere", streams: []string{"some tool\n"}, want: "some tool"},
		{name: "empty", streams: []string{"", ""}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickVersion(tc.streams...); got != tc.want {
				t.Fatalf("pickVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The compose backend shells out to `docker compose`, a separate CLI plugin, so
// it needs its own probe: a docker binary alone cannot start container
// services. Assert the table rather than running doctor, which would shell out
// to every real tool on the machine.
func TestDoctorProbesDockerComposePlugin(t *testing.T) {
	tool := doctorToolNamed(t, "docker-compose")
	if got := tool.binary(); got != "docker" {
		t.Fatalf("binary() = %q, want %q", got, "docker")
	}
	if got := strings.Join(tool.args, " "); got != "compose version" {
		t.Fatalf("args = %q, want %q", got, "compose version")
	}
	if tool.missingHint == "" {
		t.Fatal("docker-compose probe needs a missingHint: `docker` absent is not a missing plugin")
	}
}

// process-compose rejects `--version`; only the `version` subcommand answers.
func TestDoctorProbesProcessComposeWithVersionSubcommand(t *testing.T) {
	tool := doctorToolNamed(t, "process-compose")
	if got := strings.Join(tool.args, " "); got != "version" {
		t.Fatalf("args = %q, want %q", got, "version")
	}
}

func TestDoctorToolsDefaultBinaryToName(t *testing.T) {
	for _, tool := range doctorTools() {
		if tool.bin == "" && tool.binary() != tool.name {
			t.Fatalf("binary() = %q, want %q", tool.binary(), tool.name)
		}
	}
}

// A docker without the compose plugin explains itself only on stderr; a
// failed probe must carry that line, not just "exit status 1".
func TestCommandVersionSurfacesStderrOnFailure(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	script := "#!/bin/sh\necho 'usage noise'\necho 'docker: unknown command: docker compose' >&2\nexit 1\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(probe) error = %v", err)
	}
	_, err := commandVersion(context.Background(), probe, nil)
	if err == nil {
		t.Fatal("commandVersion() error is nil")
	}
	if !strings.Contains(err.Error(), "docker: unknown command: docker compose") {
		t.Fatalf("commandVersion() error = %q, want the stderr line", err)
	}
}

// git below 2.42 is a warning naming the git jj slots need, 2.48, not an
// error: no manifest can declare a jj slot yet. A git from 2.42 to 2.47 gets
// no warning from tool.git, as before 2.48 was needed, so a stack that does
// not use jj gets none; tool.jj warns of it once jj can create slots.
func TestDoctorWarnsOfGitBelowTheJJFloor(t *testing.T) {
	tool := doctorToolNamed(t, "git")
	if tool.short == nil {
		t.Fatal("the git probe checks no version floor")
	}
	for _, version := range []string{"git version 2.50.1 (Apple Git-155)", "git version 2.47.2", "git version 2.42.0"} {
		if detail, hint := tool.short(version); detail != "" || hint != "" {
			t.Fatalf("short(%s) = %q, %q; want nothing", version, detail, hint)
		}
	}
	detail, hint := tool.short("git version 2.41.0")
	if detail != "git version 2.41.0 is older than 2.48, which jj slots need" || !strings.Contains(hint, "Upgrade git to 2.48 or later") {
		t.Fatalf("short(2.41.0) = %q, %q; want the 2.48 jj slots need", detail, hint)
	}
}

// tool.jj reports jj's version and its colocate capability, and warns
// rather than fails without them, or when the git jj slots would run is older
// than 2.48 and cannot open the cache jj adds them to.
func TestDoctorJJCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   jjdriver.Capabilities
		status doctorStatus
		detail string
		hint   string
	}{
		{
			name:   "supported",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1-f8f808f", Git: "git version 2.50.1"},
			status: doctorOK,
			detail: "jj 0.45.1-f8f808f (workspace add --colocate)",
		},
		{
			name:   "missing",
			caps:   jjdriver.Capabilities{JJErr: errors.New("jj is not on PATH")},
			status: doctorWarn,
			detail: "jj is not on PATH",
			hint:   "brew install --HEAD jj",
		},
		{
			name:   "no colocate",
			caps:   jjdriver.Capabilities{JJ: "jj 0.34.0", JJErr: errors.New("jj 0.34.0 has no `jj workspace add --colocate`")},
			status: doctorWarn,
			detail: "jj 0.34.0 has no `jj workspace add --colocate`",
			hint:   "brew install --HEAD jj",
		},
		{
			name:   "windows",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1", OSErr: errors.New("jj slots are not supported on Windows yet")},
			status: doctorWarn,
			detail: "jj 0.45.1: jj slots are not supported on Windows yet",
		},
		{
			name:   "git 2.48",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.48.0"},
			status: doctorOK,
			detail: "jj 0.45.1 (workspace add --colocate)",
		},
		{
			name:   "git without relative worktrees",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.47.2"},
			status: doctorWarn,
			detail: "jj 0.45.1 (workspace add --colocate), but git version 2.47.2 is older than 2.48, which jj slots need",
			hint:   "Upgrade git to 2.48 or later before using jj slots",
		},
		{
			name:   "git below the probe's floor",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1", Git: "git version 2.41.0", GitErr: errors.New("git version 2.41.0 is older than 2.42")},
			status: doctorWarn,
			detail: "jj 0.45.1 (workspace add --colocate), but git version 2.41.0 is older than 2.48, which jj slots need",
			hint:   "Upgrade git to 2.48 or later before using jj slots",
		},
		{
			name:   "git did not answer",
			caps:   jjdriver.Capabilities{JJ: "jj 0.45.1", GitErr: errors.New("git --version failed: exit status 1")},
			status: doctorWarn,
			detail: "jj 0.45.1 (workspace add --colocate), but git reported no version, and jj slots need 2.48 or later",
			hint:   "Upgrade git to 2.48 or later before using jj slots",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := jjDoctorCheck(tc.caps)
			if check.Name != "tool.jj" || check.Status != tc.status || check.Detail != tc.detail || !strings.Contains(check.Hint, tc.hint) {
				t.Fatalf("jjDoctorCheck() = %#v, want %s %q with hint %q", check, tc.status, tc.detail, tc.hint)
			}
		})
	}
}

func doctorToolNamed(t *testing.T, name string) doctorTool {
	t.Helper()
	for _, tool := range doctorTools() {
		if tool.name == name {
			return tool
		}
	}
	t.Fatalf("doctorTools() is missing the %s probe", name)
	return doctorTool{}
}

func writeDoctorManifest(t *testing.T, root string, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}
}

func doctorCheckStatus(report doctorReport, name string) doctorStatus {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Status
		}
	}
	return ""
}
