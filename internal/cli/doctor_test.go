package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	var tool doctorTool
	for _, candidate := range doctorTools() {
		if candidate.name == "docker-compose" {
			tool = candidate
			break
		}
	}
	if tool.name == "" {
		t.Fatal("doctorTools() is missing the docker-compose probe")
	}
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

func TestDoctorToolsDefaultBinaryToName(t *testing.T) {
	for _, tool := range doctorTools() {
		if tool.bin == "" && tool.binary() != tool.name {
			t.Fatalf("binary() = %q, want %q", tool.binary(), tool.name)
		}
	}
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
