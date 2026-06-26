package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// TestRESTParityEndpointsRequireBearerToken proves every REST endpoint
// added for GraphQL parity is auth()-wrapped. Each entry hits a configured
// operator without the bearer header and expects 401.
func TestRESTParityEndpointsRequireBearerToken(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "secret-token"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/gitops/topology", ""},
		{http.MethodGet, "/sources/x/diff", ""},
		{http.MethodPost, "/workspaces/preflight", `{"template":"workspaces/dev-pr"}`},
		{http.MethodPost, "/workspaces/x/sources/y/fetch", ""},
		{http.MethodPost, "/workspaces/x/sources/y/pull", ""},
		{http.MethodPost, "/workspaces/x/sources/y/push", `{}`},
		{http.MethodGet, "/workspaces/x/sources/y/diff", ""},
		{http.MethodPost, "/workspaces/x/sources/y/merge", `{"ref":"main"}`},
		{http.MethodPost, "/workspaces/x/sources/y/rebase", `{"ref":"main"}`},
		{http.MethodPost, "/workspaces/x/sources/y/merge-abort", ""},
		{http.MethodPost, "/workspaces/x/sources/y/rebase-abort", ""},
		{http.MethodPost, "/workspaces/x/sources/y/rebase-continue", ""},
		{http.MethodPost, "/workspaces/x/sources/y/publish", `{"remote":"origin"}`},
		{http.MethodGet, "/templates", ""},
		{http.MethodGet, "/templates/workspaces/dev-pr", ""},
		{http.MethodPost, "/tokens/mint", `{"actor":"u","ttl":"1h"}`},
		{http.MethodPost, "/tokens/route", `{"actor":"u","service":"x","ttl":"1h"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body *bytes.Reader
			if tc.body == "" {
				body = bytes.NewReader(nil)
			} else {
				body = bytes.NewReader([]byte(tc.body))
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			// no Authorization header
			rr := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRESTGitOpsTopology(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	resp := doREST[api.GitOpsTopologyResponse](t, server, http.MethodGet, "/gitops/topology", nil)
	if resp.Name != "test" {
		t.Fatalf("topology.name = %q, want test", resp.Name)
	}
	if len(resp.Sources) != 0 || len(resp.Workspaces) != 0 {
		t.Fatalf("topology not empty: %#v", resp)
	}
}

func TestRESTWorkspacePreflightFlagsMissingRequired(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	templateRoot := filepath.Join(root, ".templates", "workspaces", "dev-pr")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(`_subdirectory: template
_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
`), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	body := []byte(`{"template":"workspaces/dev-pr"}`)
	resp := doREST[api.WorkspaceCreatePreflightResponse](t, server, http.MethodPost, "/workspaces/preflight", body)
	if resp.OK {
		t.Fatalf("preflight OK = true, want false (topic is required)")
	}
	if len(resp.MissingRequired) != 1 || resp.MissingRequired[0] != "topic" {
		t.Fatalf("MissingRequired = %v, want [topic]", resp.MissingRequired)
	}
}

func TestRESTTemplatesAndTemplateRef(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	templateRoot := filepath.Join(root, ".templates", "workspaces", "dev-pr")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(`_subdirectory: template
_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
`), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	list := doREST[api.TemplateListResponse](t, server, http.MethodGet, "/templates", nil)
	if len(list.Nodes) != 1 || list.Nodes[0].Ref != "workspaces/dev-pr" {
		t.Fatalf("templates list = %+v, want one workspaces/dev-pr", list)
	}
	desc := doREST[api.TemplateDescriptor](t, server, http.MethodGet, "/templates/workspaces/dev-pr", nil)
	if desc.Kind != "workspace" || desc.Name != "dev-pr" {
		t.Fatalf("template descriptor = %+v, want workspace dev-pr", desc)
	}
}

func TestRESTMintConnectionToken(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, JWTSecret: "explicit-secret-1234"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	body := []byte(`{"actor":"agent-1","scope":["service:read","service:up"],"ttl":"30m"}`)
	resp := doREST[api.ConnectionTokenResponse](t, server, http.MethodPost, "/tokens/mint", body)
	if resp.Actor != "agent-1" || resp.Token == "" {
		t.Fatalf("token response = %+v, want non-empty token for agent-1", resp)
	}
	// The minted token is an operator-API token carrying the requested scope.
	claims, err := server.tokens.Verify(resp.Token, audienceOperator)
	if err != nil {
		t.Fatalf("Verify(operator) error = %v", err)
	}
	if strings.Join(claims.Scope, ",") != "service:read,service:up" {
		t.Fatalf("scope = %#v, want [service:read service:up]", claims.Scope)
	}
}

func TestRESTMintRouteToken(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, JWTSecret: "explicit-secret-1234"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	body := []byte(`{"actor":"agent-1","service":"agent-x","ttl":"60s"}`)
	resp := doREST[api.ConnectionTokenResponse](t, server, http.MethodPost, "/tokens/route", body)
	if resp.Actor != "agent-1" || resp.Token == "" {
		t.Fatalf("token response = %+v, want non-empty token for agent-1", resp)
	}
	// The token is bound to that one service's audience and nothing else.
	if _, err := server.tokens.Verify(resp.Token, serviceAudience("agent-x")); err != nil {
		t.Fatalf("Verify(svc:agent-x) error = %v", err)
	}
	if _, err := server.tokens.Verify(resp.Token, audienceOperator); err == nil {
		t.Fatal("route token verified as an operator token; want audience rejection")
	}
}

func TestRESTMintRouteTokenRejectsEmptyService(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	body := []byte(`{"actor":"agent-1","service":""}`)
	req := httptest.NewRequest(http.MethodPost, "/tokens/route", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "service") {
		t.Fatalf("body = %q, want mention of service", rr.Body.String())
	}
}

func TestRESTMintConnectionTokenRejectsEmptyActor(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	body := []byte(`{"actor":""}`)
	req := httptest.NewRequest(http.MethodPost, "/tokens/mint", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "actor") {
		t.Fatalf("body = %q, want mention of actor", rr.Body.String())
	}
}

func TestRESTGitOpsTopologyRejectsInvalidWithCommits(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	req := httptest.NewRequest(http.MethodGet, "/gitops/topology?with_commits=-5", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// doREST runs the given request against the server's mux without auth
// (Config.Token is empty by default in these tests). The unauthorized-access
// guard is tested by TestRESTParityEndpointsRequireBearerToken; everything
// else exercises the handler logic.
func doREST[T any](t *testing.T, server *Server, method, path string, body []byte) T {
	t.Helper()
	var reqBody *bytes.Reader
	if body == nil {
		reqBody = bytes.NewReader(nil)
	} else {
		reqBody = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reqBody)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code >= 300 {
		t.Fatalf("REST %s %s status = %d, body = %s", method, path, rr.Code, rr.Body.String())
	}
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("Unmarshal: %v, body = %s", err, rr.Body.String())
	}
	return out
}
