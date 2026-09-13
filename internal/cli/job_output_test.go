package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJobRunPrintsBufferedOutputOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/jobs/codegen/run" {
			t.Errorf("request = %s %s, want POST /jobs/codegen/run", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"run-1","root_job":"codegen","status":"succeeded","started_at":"2026-09-13T10:00:00Z","nodes":[{"name":"codegen","kind":"job","status":"succeeded"}],"output":"generated\n"}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--operator", server.URL, "job", "run", "codegen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v; stderr = %q", err, stderr.String())
	}
	if got := stdout.String(); got != "generated\n" {
		t.Fatalf("stdout = %q, want one copy of %q", got, "generated\n")
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}
