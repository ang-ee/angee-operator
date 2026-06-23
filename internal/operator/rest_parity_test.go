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
)

// TestTemplateRefRejectsAbsoluteAndTraversal locks in the
// admin-bearer-info-disclosure defense added after the parity commit
// review: REST clients (and the underlying Platform method) must not
// be able to enumerate arbitrary copier.yml files on the host even
// when the admin bearer is presented.
//
// Two layers defend the surface: (a) Go's net/http mux normalizes
// unencoded `..` and `//` sequences (the request is redirected away
// from /templates before reaching the handler), and (b) Platform.Template
// rejects the URL-encoded variants the mux can't see. This test
// exercises both.
func TestTemplateRefRejectsAbsoluteAndTraversal(t *testing.T) {
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

	cases := []struct {
		name string
		path string
	}{
		// Unencoded variants: the stdlib mux normalizes these to a clean
		// path that no longer matches the /templates route, so we expect
		// a redirect (307) — the handler never sees the malicious ref.
		{"unencoded double slash", "/templates//etc/passwd"},
		{"unencoded dot-dot", "/templates/workspaces/../../etc"},
		// Encoded variants: bypass the mux's path cleaning and hit our
		// handler with the literal traversal string. Platform.Template
		// rejects them with a typed InvalidInputError (HTTP 400).
		{"encoded absolute path", "/templates/%2Fetc%2Fpasswd"},
		{"encoded dot-dot", "/templates/workspaces/%2E%2E/%2E%2E/etc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				t.Fatalf("status = 200 — handler returned a descriptor for malicious ref (body=%s)", rr.Body.String())
			}
			// Either the mux normalized the path away (3xx redirect to a
			// route that doesn't exist) or our handler rejected with 400.
			// Both are acceptable defenses; 200 is the only forbidden outcome.
			if rr.Code < 300 || rr.Code >= 600 {
				t.Fatalf("status = %d, want a non-success status (3xx/4xx)", rr.Code)
			}
		})
	}
}

// TestGitOpsTopologyValidationParity verifies that REST and GraphQL
// reject the same negative `withCommits` values via the same Platform
// validation path. Prevents the previous drift where REST returned
// 400 and GraphQL silently coerced.
func TestGitOpsTopologyValidationParity(t *testing.T) {
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

	// REST: GET with negative -> 400
	req := httptest.NewRequest(http.MethodGet, "/gitops/topology?with_commits=-5", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("REST status = %d, want 400", rr.Code)
	}

	// GraphQL: same negative -> error in response (operator wraps as 200 OK
	// with an `errors` array per spec).
	gqlBody, _ := json.Marshal(map[string]any{
		"query": `query Q($w: Int) { gitOpsTopology(withCommits: $w) { name } }`,
		"variables": map[string]any{
			"w": -5,
		},
	})
	gqlReq := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(gqlBody))
	gqlReq.Header.Set("Content-Type", "application/json")
	gqlRR := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(gqlRR, gqlReq)
	if gqlRR.Code != http.StatusOK {
		t.Fatalf("GraphQL HTTP status = %d, want 200 (errors live in body)", gqlRR.Code)
	}
	var resp struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(gqlRR.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(resp.Errors) == 0 {
		t.Fatalf("GraphQL response had no errors for negative withCommits: %s", gqlRR.Body.String())
	}
}

// TestRESTAndGraphQLTemplateMatch asserts that the REST and GraphQL
// `template(ref)` surfaces return the same logical descriptor for the
// same input. Locks in the parity contract documented in the commit
// message.
func TestRESTAndGraphQLTemplateMatch(t *testing.T) {
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

	restReq := httptest.NewRequest(http.MethodGet, "/templates/workspaces/dev-pr", nil)
	restRR := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(restRR, restReq)
	if restRR.Code != http.StatusOK {
		t.Fatalf("REST status = %d, want 200: %s", restRR.Code, restRR.Body.String())
	}
	var rest map[string]any
	if err := json.Unmarshal(restRR.Body.Bytes(), &rest); err != nil {
		t.Fatalf("REST unmarshal: %v", err)
	}

	gqlBody, _ := json.Marshal(map[string]any{
		"query": `{ template(id: "workspaces/dev-pr") { ref kind name inputs { name required } } }`,
	})
	gqlReq := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(gqlBody))
	gqlReq.Header.Set("Content-Type", "application/json")
	gqlRR := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(gqlRR, gqlReq)
	if gqlRR.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d, want 200: %s", gqlRR.Code, gqlRR.Body.String())
	}
	var gql struct {
		Data struct {
			Template map[string]any `json:"template"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(gqlRR.Body.Bytes(), &gql); err != nil {
		t.Fatalf("GraphQL unmarshal: %v", err)
	}
	if len(gql.Errors) > 0 {
		t.Fatalf("GraphQL errors: %+v", gql.Errors)
	}

	// Both surfaces should agree on the load-bearing fields. REST returns
	// the full struct including `path`; GraphQL selects a subset. Compare
	// the fields actually requested in the GraphQL query.
	if rest["ref"] != gql.Data.Template["ref"] {
		t.Fatalf("ref mismatch: REST=%v GraphQL=%v", rest["ref"], gql.Data.Template["ref"])
	}
	if rest["kind"] != gql.Data.Template["kind"] {
		t.Fatalf("kind mismatch: REST=%v GraphQL=%v", rest["kind"], gql.Data.Template["kind"])
	}
	if rest["name"] != gql.Data.Template["name"] {
		t.Fatalf("name mismatch: REST=%v GraphQL=%v", rest["name"], gql.Data.Template["name"])
	}
}

// TestRESTBodySizeLimit asserts that decode's MaxBytesReader cap rejects
// hostile oversize JSON. The payload is real JSON whose `template`
// value is large enough to push the decoder past the cap, so we exercise
// the MaxBytesReader path (not the JSON parser's first-byte rejection).
func TestRESTBodySizeLimit(t *testing.T) {
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

	payload := `{"template":"` + strings.Repeat("a", maxRESTBodyBytes+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/workspaces/preflight", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}
