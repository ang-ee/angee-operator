package operator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

func sp(s string) *string { return &s }

// TestRESTServiceListFilter exercises REST filter parity: a `?query=<json>` spec
// filters the list and returns {nodes,totalCount}; unknown fields and malformed
// JSON are 400s.
func TestRESTServiceListFilter(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  api:
    runtime: container
    image: nginx:latest
  web:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)

	lq := api.ListQuery{Filter: &api.ListFilter{Fields: map[string]api.FieldComparison{"name": {Eq: sp("api")}}}}
	raw, _ := json.Marshal(lq)
	resp := doREST[api.ServiceListResponse](t, server, http.MethodGet, "/services?query="+url.QueryEscape(string(raw)), nil)
	if resp.TotalCount != 1 || len(resp.Nodes) != 1 || resp.Nodes[0].Name != "api" {
		t.Fatalf("filtered services = %+v, want only api", resp)
	}

	// Unknown filter field -> 400 (platform query.Validate).
	bad, _ := json.Marshal(api.ListQuery{Filter: &api.ListFilter{Fields: map[string]api.FieldComparison{"bogus": {Eq: sp("x")}}}})
	if code := restStatus(server, "/services?query="+url.QueryEscape(string(bad))); code != http.StatusBadRequest {
		t.Fatalf("unknown filter field status = %d, want 400", code)
	}

	// Malformed query JSON -> 400 (parseListQuery).
	if code := restStatus(server, "/services?query=not-json"); code != http.StatusBadRequest {
		t.Fatalf("malformed query status = %d, want 400", code)
	}
}

func restStatus(server *Server, path string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	return rr.Code
}
