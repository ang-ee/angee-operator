package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
)

func TestNewServerRequiresTokenForNonLoopbackBind(t *testing.T) {
	_, err := NewServer(Config{Root: t.TempDir(), Bind: "0.0.0.0", Port: 9000})
	if err == nil {
		t.Fatal("NewServer() error = nil, want token requirement")
	}
}

func TestNewServerResolvesProjectRootToControlRoot(t *testing.T) {
	projectRoot := t.TempDir()
	controlRoot := filepath.Join(projectRoot, ".angee")
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	server, err := NewServer(Config{Root: projectRoot, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.config.Root != controlRoot {
		t.Fatalf("server root = %q, want %q", server.config.Root, controlRoot)
	}
}

func TestGraphQLStackStatus(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  api:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ stackStatus { name services { name runtime status } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	status := resp.Data["stackStatus"].(map[string]any)
	if status["name"] != "test" {
		t.Fatalf("stackStatus.name = %v, want test", status["name"])
	}
	services := status["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("services length = %d, want 1", len(services))
	}
	service := services[0].(map[string]any)
	if service["name"] != "api" || service["runtime"] != "container" || service["status"] != "declared" {
		t.Fatalf("service = %#v, want declared api container", service)
	}
}

func TestGraphQLWorkspaceStatus(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
sources:
  app:
    kind: git
    repo: https://example.invalid/app.git
workspaces:
  feat:
    template: workspaces/dev-pr
    inputs:
      topic: feature
    sources:
      app:
        source: app
        mode: worktree
        branch: workspace/feat
        ref: main
        subpath: app
services:
  worker:
    runtime: local
    command: ["true"]
    mounts: ["workspace://feat:/workspace"]
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ workspaceStatus(name: "feat") { name state exists inputs sources { slot source kind mode state pushed } mountedBy { kind name field } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	status := resp.Data["workspaceStatus"].(map[string]any)
	if status["name"] != "feat" || status["state"] != "missing" || status["exists"] != false {
		t.Fatalf("workspaceStatus = %#v, want missing feat", status)
	}
	sources := status["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources length = %d, want 1", len(sources))
	}
	source := sources[0].(map[string]any)
	if source["slot"] != "app" || source["source"] != "app" || source["kind"] != "git" || source["mode"] != "worktree" || source["state"] != "missing" {
		t.Fatalf("source status = %#v, want missing app git worktree", source)
	}
	mountedBy := status["mountedBy"].([]any)
	if len(mountedBy) != 1 {
		t.Fatalf("mountedBy length = %d, want 1", len(mountedBy))
	}
	ref := mountedBy[0].(map[string]any)
	if ref["kind"] != "service" || ref["name"] != "worker" || ref["field"] != "mounts" {
		t.Fatalf("mountedBy = %#v, want worker mounts ref", ref)
	}
}

func TestGraphQLServiceInit(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `mutation($input: ServiceInput!) {
			serviceInit(input: $input) { status name }
		}`,
		"variables": map[string]any{
			"input": map[string]any{
				"name":  "web",
				"image": "nginx:latest",
				"env": []map[string]string{
					{"key": "FOO", "value": "bar"},
				},
				"ports": []string{"8080:80"},
			},
		},
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	result := resp.Data["serviceInit"].(map[string]any)
	if result["status"] != "created" || result["name"] != "web" {
		t.Fatalf("serviceInit = %#v, want created web", result)
	}
	stack, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	service, ok := stack.Services["web"]
	if !ok {
		t.Fatalf("service web was not persisted")
	}
	if service.Image != "nginx:latest" || service.Env["FOO"] != "bar" {
		t.Fatalf("service web = %#v, want image and env persisted", service)
	}
}

func TestGraphQLStackPrepareUsesBackendFieldNames(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  web:
    runtime: local
    command: ["go", "version"]
    workdir: /tmp
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `mutation { stackPrepare { processCompose } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	prepared := resp.Data["stackPrepare"].(map[string]any)
	processCompose := prepared["processCompose"].(map[string]any)
	processes := processCompose["processes"].(map[string]any)
	web := processes["web"].(map[string]any)
	if web["working_dir"] != "/tmp" {
		t.Fatalf("working_dir = %v, want /tmp; full process = %#v", web["working_dir"], web)
	}
	if _, exists := web["WorkingDir"]; exists {
		t.Fatalf("process exposes Go field name WorkingDir: %#v", web)
	}
}

func TestGraphQLRequiresPOST(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/graphql?query={health{status}}", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /graphql status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestGraphQLRejectsSimpleBrowserContentTypes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"mutation { stackDown { status } }"}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain GraphQL status = %d, want %d", rr.Code, http.StatusUnsupportedMediaType)
	}
}

func TestGraphQLRejectsCrossOriginBrowserPosts(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"{ health { status } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin GraphQL status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestGraphQLBodySizeLimit(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := bytes.NewBuffer(make([]byte, maxGraphQLBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/graphql")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized GraphQL status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestGraphQLJSONBodySizeLimitIncludesTrailingBytes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := bytes.NewBufferString(`{"query":"{ health { status } }"}`)
	body.Write(bytes.Repeat([]byte(" "), maxGraphQLBodyBytes))
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON GraphQL status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

func writeTestStack(t *testing.T, root, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}
}

type graphQLTestResponse struct {
	Data   map[string]any `json:"data"`
	Errors []any          `json:"errors,omitempty"`
}

func doGraphQL(t *testing.T, server *Server, body map[string]any) graphQLTestResponse {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GraphQL HTTP status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp graphQLTestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v: %s", err, rr.Body.String())
	}
	return resp
}
