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
