package platformclient

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestBearerOptionAuthenticatesEveryRemoteTransportWithoutLoggingToken(t *testing.T) {
	const token = "private-bearer-not-for-urls-or-logs"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("remote transport omitted configured bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.String(), token) {
			t.Error("bearer appeared in request URL")
		}
		switch r.URL.Path {
		case "/sources/example/status":
			_ = json.NewEncoder(w).Encode(api.SourceState{Name: "example"})
		case "/job-runs/one":
			_ = json.NewEncoder(w).Encode(api.JobRunOperation{ID: "one", Artifact: &api.BackupArtifact{}})
		case "/backups/one/archive":
			_, _ = io.WriteString(w, "archive")
		case "/stack/logs", "/stack/up/stream":
			_, _ = io.WriteString(w, "output\n")
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	var logs synchronizedBuffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	client := New(server.URL, WithBearerToken(token))
	if _, err := client.SourceStatus(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	stream, err := client.StackLogs(ctx, nil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	var foreground bytes.Buffer
	if err := client.StackUpForeground(ctx, nil, false, &foreground, &foreground); err != nil {
		t.Fatal(err)
	}
	archive, _, err := client.BackupArchive(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, archive); err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("configured bearer appeared in transport logs")
	}
}
