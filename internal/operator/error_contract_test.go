package operator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// Sync base reports each failure with the same code, cause and context on
// GraphQL (extensions) and REST (body and status), with the request's ID, and
// logs it; a sync that can proceed is unaffected.
func TestSyncBaseErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(t *testing.T, root, slotPath, cache string)
		wantStatus int
		wantExt    map[string]any
		wantPrefix string
	}{
		{
			name: "dirty slot",
			prepare: func(t *testing.T, _, slotPath, _ string) {
				writeTestFile(t, filepath.Join(slotPath, "scratch.txt"), "work in progress\n")
			},
			wantStatus: http.StatusConflict,
			wantExt: map[string]any{"code": "PRECONDITION_FAILED", "cause": "uncommitted_changes", "operation": "workspace.sync-base",
				"workspace": "feature-a", "slot": "app", "source": "app", "paths": []any{"scratch.txt"}, "hint": "Commit or restore the changes, then retry."},
			wantPrefix: `Sync base for workspace "feature-a": source slot "app" has uncommitted changes in scratch.txt.`,
		},
		{
			name: "missing slot",
			prepare: func(t *testing.T, root, _, _ string) {
				declareOperatorSlot(t, root, "feature-a", "lib", manifest.WorkspaceSource{Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a-lib", Ref: "main", Subpath: "lib"})
			},
			wantStatus: http.StatusConflict,
			wantExt: map[string]any{"code": "PRECONDITION_FAILED", "cause": "slot_missing", "operation": "workspace.sync-base",
				"workspace": "feature-a", "slot": "lib", "hint": "Run `angee workspace repair feature-a`."},
			wantPrefix: `Sync base for workspace "feature-a": source slot "lib" is missing at `,
		},
		{
			name: "ssh unavailable",
			prepare: func(t *testing.T, _, _, cache string) {
				runTestGit(t, cache, "remote", "set-url", "fork", "git@github-deploy:org/app.git")
				operatorPathWithoutSSH(t)
			},
			wantStatus: http.StatusInternalServerError,
			wantExt: map[string]any{"code": "GIT_FAILED", "cause": "fetch_failed", "operation": "workspace.sync-base",
				"workspace": "feature-a", "slot": "app", "source": "app", "remote": "git@github-deploy:org/app.git"},
			wantPrefix: `Sync base for workspace "feature-a": fetching source "app" (git@github-deploy:org/app.git) failed. git: `,
		},
		{
			name: "unreachable remote",
			prepare: func(t *testing.T, _, _, cache string) {
				runTestGit(t, cache, "remote", "set-url", "fork", "https://nonexistent.invalid/app.git")
				t.Setenv("ANGEE_GIT_TIMEOUT", "30s")
			},
			wantStatus: http.StatusInternalServerError,
			wantExt: map[string]any{"code": "GIT_FAILED", "cause": "fetch_failed", "operation": "workspace.sync-base",
				"workspace": "feature-a", "slot": "app", "source": "app", "remote": "https://nonexistent.invalid/app.git"},
			wantPrefix: `Sync base for workspace "feature-a": fetching source "app" (https://nonexistent.invalid/app.git) failed. git: fatal: unable to access`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, slotPath, cache := setupOperatorGitWorkspace(t)
			tc.prepare(t, root, slotPath, cache)
			var logs bytes.Buffer
			server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 28091, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			t.Cleanup(server.Close)

			req := graphQLRequest(t, `mutation { workspaceSyncBase(name: "feature-a") { name } }`)
			req.Header.Set("X-Request-ID", "web-42")
			rr := serveRequest(server, req)
			var resp struct {
				Errors []struct {
					Message    string         `json:"message"`
					Extensions map[string]any `json:"extensions"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Errors) != 1 {
				t.Fatalf("GraphQL response = %s (%v), want one error", rr.Body.String(), err)
			}
			gqlErr := resp.Errors[0]
			if !strings.HasPrefix(gqlErr.Message, tc.wantPrefix) {
				t.Fatalf("GraphQL message = %q, want it to start %q", gqlErr.Message, tc.wantPrefix)
			}
			for key, want := range tc.wantExt {
				if got := gqlErr.Extensions[key]; !reflect.DeepEqual(got, want) {
					t.Errorf("extensions[%q] = %#v, want %#v (all: %#v)", key, got, want, gqlErr.Extensions)
				}
			}
			for _, legacy := range []string{"kind", "name", "field", "reason"} {
				if got, ok := gqlErr.Extensions[legacy]; ok {
					t.Errorf("extensions[%q] = %#v, want the pre-contract key gone", legacy, got)
				}
			}
			if gqlErr.Extensions["request_id"] != "web-42" || rr.Header().Get("X-Request-ID") != "web-42" {
				t.Errorf("request_id = %v, header %q; want the incoming web-42", gqlErr.Extensions["request_id"], rr.Header().Get("X-Request-ID"))
			}
			if line := logLine(logs.String(), "graphql operation failed"); !strings.Contains(line, "req=web-42") || !strings.Contains(line, "code="+tc.wantExt["code"].(string)) {
				t.Errorf("log = %q, want a graphql operation failed record with the request id and code", logs.String())
			}

			logs.Reset()
			rest := serveREST(server, http.MethodPost, "/workspaces/feature-a/sync-base", `{"method":"merge"}`)
			var body api.ErrorResponse
			if err := json.Unmarshal(rest.Body.Bytes(), &body); err != nil {
				t.Fatalf("Unmarshal(REST error) error = %v: %s", err, rest.Body.String())
			}
			if rest.Code != tc.wantStatus || body.Code != tc.wantExt["code"] || body.Cause != tc.wantExt["cause"] || body.Error != gqlErr.Message ||
				body.Operation != "workspace.sync-base" || body.RequestID == "" || body.RequestID != rest.Header().Get("X-Request-ID") {
				t.Fatalf("REST = %d %+v, want %d with the GraphQL error's code, cause, message and its request id", rest.Code, body, tc.wantStatus)
			}
			if line := logLine(logs.String(), "operation failed"); !strings.Contains(line, "req="+body.RequestID) || !strings.Contains(line, "code="+body.Code) || !strings.Contains(line, "path=/workspaces/feature-a/sync-base") {
				t.Errorf("REST log = %q, want an operation failed record with the request id, code and path", logs.String())
			}
		})
	}

	t.Run("success", func(t *testing.T) {
		root, _, _, _ := setupOperatorGitWorkspace(t)
		server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 28092})
		if err != nil {
			t.Fatalf("NewServer() error = %v", err)
		}
		t.Cleanup(server.Close)
		resp := doGraphQL(t, server, map[string]any{"query": `mutation { workspaceSyncBase(name: "feature-a") { name } }`})
		if len(resp.Errors) != 0 || resp.Data["workspaceSyncBase"] == nil {
			t.Fatalf("GraphQL workspaceSyncBase = %#v, want data and no errors", resp)
		}
	})
}

// Every REST error body has a code, including ones written before a request
// reaches the platform, and an unusable X-Request-ID is replaced.
func TestRESTErrorsAlwaysCarryACodeAndRequestID(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	var logs bytes.Buffer
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 28093, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		method, path, contentType string
		wantStatus                int
		wantCode                  string
	}{
		{http.MethodPost, "/sources/nope/fetch", "", http.StatusNotFound, "NOT_FOUND"},
		{http.MethodPost, "/graphql", "text/plain", http.StatusUnsupportedMediaType, "INVALID_INPUT"},
	} {
		req := httptestRequest(tc.method, tc.path, "")
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		req.Header.Set("X-Request-ID", "bad id\nwith newline")
		rr := serveRequest(server, req)
		var body api.ErrorResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: Unmarshal error = %v: %s", tc.method, tc.path, err, rr.Body.String())
		}
		id := rr.Header().Get("X-Request-ID")
		if rr.Code != tc.wantStatus || body.Code != tc.wantCode || body.RequestID != id || len(id) != 8 {
			t.Fatalf("%s %s = %d %+v (header id %q), want %d %s with a generated request id", tc.method, tc.path, rr.Code, body, id, tc.wantStatus, tc.wantCode)
		}
		// Written by the handler or by the service, the failure is logged.
		if line := logLine(logs.String(), "operation failed"); !strings.Contains(line, "req="+id) || !strings.Contains(line, "code="+tc.wantCode) {
			t.Fatalf("%s %s log = %q, want an operation failed record", tc.method, tc.path, logs.String())
		}
		logs.Reset()
	}

	// A query gqlgen rejects itself keeps gqlgen's code and gains the request ID.
	rr := serveRequest(server, graphQLRequest(t, `{ noSuchField }`))
	var resp struct {
		Errors []struct {
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Errors) == 0 {
		t.Fatalf("GraphQL response = %s (%v), want an error", rr.Body.String(), err)
	}
	if ext := resp.Errors[0].Extensions; ext["code"] != "GRAPHQL_VALIDATION_FAILED" || ext["request_id"] != rr.Header().Get("X-Request-ID") {
		t.Fatalf("GraphQL validation error extensions = %#v, want gqlgen's code and the request id", ext)
	}
}

// The operator masks credentials once more on the way out, for text that
// reached an error without passing a redacting source.
func TestClassifiedErrorRedactsOnTheWayOut(t *testing.T) {
	c := classifyServiceError(fmt.Errorf("clone https://user:s3cret@example.com/x.git: boom"))
	if strings.Contains(c.message, "s3cret") || !strings.Contains(c.message, "https://***@example.com/x.git") || c.code != "INTERNAL" {
		t.Fatalf("classified = %+v, want the credential masked", c)
	}
}

func graphQLRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	data, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return httptestRequest(http.MethodPost, "/graphql", string(data))
}

func httptestRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serveRequest(server *Server, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	return rr
}

// logLine returns the first line of logs containing msg="<msg>".
func logLine(logs, msg string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			return line
		}
	}
	return ""
}

// operatorPathWithoutSSH leaves only git on PATH, and no GIT_SSH_COMMAND.
func operatorPathWithoutSSH(t *testing.T) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git) error = %v", err)
	}
	bin := t.TempDir()
	if err := os.Symlink(gitBin, filepath.Join(bin, "git")); err != nil {
		t.Fatalf("Symlink(git) error = %v", err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_SSH_COMMAND", "")
	if err := os.Unsetenv("GIT_SSH_COMMAND"); err != nil {
		t.Fatalf("Unsetenv(GIT_SSH_COMMAND) error = %v", err)
	}
}
