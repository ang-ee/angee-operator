package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// filesTestStack declares a single `local` source `app` rooted at the `app/`
// subdir; the files routes resolve `source=app` to that on-disk directory.
const filesTestStack = `version: 1
kind: stack
name: test
sources:
  app:
    kind: local
    path: app
`

// newFilesServer builds an operator server over a temp root whose manifest
// declares the `app` local source, seeding the source dir so reads/writes
// resolve. token is the admin bearer ("" disables auth for handler tests).
func newFilesServer(t *testing.T, token string) *Server {
	t.Helper()
	root := t.TempDir()
	writeTestStack(t, root, filesTestStack)
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatalf("MkdirAll(app) error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: token})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

// filesPath builds `/files?source=...&path=...`. source and path are query
// params (not path segments) because a file path holds slashes.
func filesPath(source, path string) string {
	q := url.Values{"source": {source}, "path": {path}}
	return "/files?" + q.Encode()
}

// TestFilesRESTEndpointsRequireBearer locks in admin-bearer auth for both the
// read and write file endpoints, mirroring the secrets routes.
func TestFilesRESTEndpointsRequireBearer(t *testing.T) {
	server := newFilesServer(t, "secret-token")

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, filesPath("app", "x.txt"), ""},
		{http.MethodPut, filesPath("app", "x.txt"), `{"content":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
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
			rr := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestFilesRESTWriteThenRead exercises the write/read roundtrip through the HTTP
// server: PUT seeds content and returns a non-empty etag; GET returns the same
// content and the matching etag.
func TestFilesRESTWriteThenRead(t *testing.T) {
	server := newFilesServer(t, "")

	ref := doREST[api.FileRef](t, server, http.MethodPut, filesPath("app", "config/app.yaml"), []byte(`{"content":"hello: world\n"}`))
	if ref.Path != "config/app.yaml" || ref.Source != "app" || ref.Etag == "" {
		t.Fatalf("write ref = %+v, want path+source+non-empty etag", ref)
	}

	got := doREST[api.FileContent](t, server, http.MethodGet, filesPath("app", "config/app.yaml"), nil)
	if got.Content != "hello: world\n" {
		t.Fatalf("content = %q, want %q", got.Content, "hello: world\n")
	}
	if got.Etag != ref.Etag {
		t.Fatalf("read etag %q != write etag %q", got.Etag, ref.Etag)
	}
	if got.Source != "app" || got.Path != "config/app.yaml" {
		t.Fatalf("content meta = %+v", got)
	}
}

// TestFilesRESTTraversalAndUnknownSourceRejected covers the two rejection paths:
// a relpath escaping the source root is a 400 (InvalidInput) and an undeclared
// source is a 404 (NotFound).
func TestFilesRESTTraversalAndUnknownSourceRejected(t *testing.T) {
	server := newFilesServer(t, "")

	// Traversal relpath -> 400.
	req := httptest.NewRequest(http.MethodPut, filesPath("app", "../escape"), bytes.NewReader([]byte(`{"content":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d, want 400: %s", rr.Code, rr.Body.String())
	}

	// Unknown source -> 404.
	rr = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, filesPath("ghost", "x.txt"), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown source status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

// TestFilesRESTEtagConflict verifies the optimistic-concurrency precondition: a
// PUT carrying a stale etag is a 409.
func TestFilesRESTEtagConflict(t *testing.T) {
	server := newFilesServer(t, "")

	doREST[api.FileRef](t, server, http.MethodPut, filesPath("app", "x.txt"), []byte(`{"content":"first"}`))

	req := httptest.NewRequest(http.MethodPut, filesPath("app", "x.txt"), bytes.NewReader([]byte(`{"content":"second","etag":"deadbeef"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
}

// TestFilesRESTAndGraphQLAgree is the REST/GraphQL parity check (mirrors
// TestSecretsRESTAndGraphQLAgreeOnList): the REST `GET /files` handler and the
// GraphQL `file(source, path)` query must return the same content and etag for
// the same input.
func TestFilesRESTAndGraphQLAgree(t *testing.T) {
	server := newFilesServer(t, "")

	// Seed a file through the REST write path.
	doREST[api.FileRef](t, server, http.MethodPut, filesPath("app", "config/app.yaml"), []byte(`{"content":"hello: world\n"}`))

	rest := doREST[api.FileContent](t, server, http.MethodGet, filesPath("app", "config/app.yaml"), nil)

	gqlBody, _ := json.Marshal(map[string]any{
		"query":     `query Q($s: String!, $p: String!) { file(source: $s, path: $p) { path source content etag } }`,
		"variables": map[string]any{"s": "app", "p": "config/app.yaml"},
	})
	gqlReq := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(gqlBody))
	gqlReq.Header.Set("Content-Type", "application/json")
	gqlRR := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(gqlRR, gqlReq)
	if gqlRR.Code != http.StatusOK {
		t.Fatalf("GraphQL status = %d (body=%s)", gqlRR.Code, gqlRR.Body.String())
	}
	var gql struct {
		Data struct {
			File api.FileContent `json:"file"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(gqlRR.Body.Bytes(), &gql); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(gql.Errors) > 0 {
		t.Fatalf("GraphQL errors: %+v", gql.Errors)
	}
	if gql.Data.File.Content != rest.Content {
		t.Fatalf("content mismatch: REST=%q GraphQL=%q", rest.Content, gql.Data.File.Content)
	}
	if gql.Data.File.Etag != rest.Etag {
		t.Fatalf("etag mismatch: REST=%q GraphQL=%q", rest.Etag, gql.Data.File.Etag)
	}
	if gql.Data.File.Path != rest.Path || gql.Data.File.Source != rest.Source {
		t.Fatalf("meta mismatch: REST=%+v GraphQL=%+v", rest, gql.Data.File)
	}
}
