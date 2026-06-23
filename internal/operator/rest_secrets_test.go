package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// TestSecretsRESTEndpointsRequireBearer locks in admin-bearer auth for
// every CRUD endpoint, including the privileged value-read.
func TestSecretsRESTEndpointsRequireBearer(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
secrets:
  db-password:
    required: true
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "secret-token"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	cases := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/secrets", ""},
		{http.MethodGet, "/secrets/db-password", ""},
		{http.MethodGet, "/secrets/db-password/value", ""},
		{http.MethodPost, "/secrets/db-password", `{"value":"x"}`},
		{http.MethodDelete, "/secrets/db-password", ""},
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
			rr := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSecretsRESTSetThenListThenRevealThenDelete(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
secrets:
  db-password:
    required: true
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	// Initial list: declared, no value.
	refs := doREST[[]api.SecretRef](t, server, http.MethodGet, "/secrets", nil)
	if len(refs) != 1 || refs[0].Name != "db-password" || refs[0].HasValue {
		t.Fatalf("initial list = %+v, want one declared+empty secret", refs)
	}

	// Set
	setRef := doREST[api.SecretRef](t, server, http.MethodPost, "/secrets/db-password", []byte(`{"value":"hunter2"}`))
	if !setRef.HasValue || setRef.EnvVar == "" {
		t.Fatalf("set response = %+v", setRef)
	}

	// List again: hasValue now true
	refs = doREST[[]api.SecretRef](t, server, http.MethodGet, "/secrets", nil)
	if !refs[0].HasValue {
		t.Fatalf("post-set list = %+v, want hasValue=true", refs)
	}

	// Reveal
	val := doREST[api.SecretValueResponse](t, server, http.MethodGet, "/secrets/db-password/value", nil)
	if val.Value != "hunter2" {
		t.Fatalf("value = %q, want hunter2", val.Value)
	}

	// Delete
	resp := doREST[map[string]string](t, server, http.MethodDelete, "/secrets/db-password", nil)
	if resp["status"] != "deleted" {
		t.Fatalf("delete response = %+v", resp)
	}

	// List: hasValue now false again
	refs = doREST[[]api.SecretRef](t, server, http.MethodGet, "/secrets", nil)
	if refs[0].HasValue {
		t.Fatalf("post-delete list = %+v, want hasValue=false", refs)
	}
}

func TestSecretsRESTSetRejectsBadName(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	req := httptest.NewRequest(http.MethodPost, "/secrets/bad%20name", bytes.NewReader([]byte(`{"value":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestSecretsRESTAndGraphQLAgreeOnList(t *testing.T) {
	// Parity-by-construction: REST list and GraphQL `secrets` query must
	// return the same set of declared secrets.
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
secrets:
  db-password:
    required: true
  jwt-key:
    generated: true
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	rest := doREST[[]api.SecretRef](t, server, http.MethodGet, "/secrets", nil)
	gqlBody, _ := json.Marshal(map[string]any{
		"query": `{ secrets { nodes { name declared hasValue required generated envVar } totalCount } }`,
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
			Secrets struct {
				Nodes      []map[string]any `json:"nodes"`
				TotalCount int              `json:"totalCount"`
			} `json:"secrets"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(gqlRR.Body.Bytes(), &gql); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(gql.Errors) > 0 {
		t.Fatalf("GraphQL errors: %+v", gql.Errors)
	}
	if len(rest) != len(gql.Data.Secrets.Nodes) || gql.Data.Secrets.TotalCount != len(rest) {
		t.Fatalf("REST count=%d GraphQL count=%d totalCount=%d", len(rest), len(gql.Data.Secrets.Nodes), gql.Data.Secrets.TotalCount)
	}
	for i, r := range rest {
		if r.Name != gql.Data.Secrets.Nodes[i]["name"] {
			t.Fatalf("name mismatch at %d: REST=%v GraphQL=%v", i, r.Name, gql.Data.Secrets.Nodes[i]["name"])
		}
	}
}
