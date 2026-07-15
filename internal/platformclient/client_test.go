package platformclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

func TestOperatorHTTPErrorPreservesStatusAndFields(t *testing.T) {
	body, err := json.Marshal(api.ErrorResponse{
		Kind:  "workspace",
		Name:  "missing",
		Error: `workspace "missing" is not declared`,
	})
	if err != nil {
		t.Fatalf("Marshal(ErrorResponse) error = %v", err)
	}

	err = operatorHTTPError(http.StatusNotFound, body)
	var notFound *RemoteNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("operatorHTTPError() = %T, want RemoteNotFound", err)
	}
	if notFound.Status != http.StatusNotFound || notFound.Body.Kind != "workspace" || notFound.Body.Name != "missing" {
		t.Fatalf("RemoteNotFound = %#v", notFound)
	}
	if got := err.Error(); !strings.Contains(got, "HTTP 404") || !strings.Contains(got, `workspace "missing" is not declared`) {
		t.Fatalf("error string = %q, want status and message", got)
	}
}

func TestServiceUpdateFromTemplateUsesDedicatedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/services/agent%2Fone/template/update" {
			t.Fatalf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		var req api.ServiceUpdateTemplateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode(request): %v", err)
		}
		if !req.DryRun || !req.Overwrite || req.Inputs["mode"] != "oauth" {
			t.Fatalf("request body = %#v", req)
		}
		_ = json.NewEncoder(w).Encode(api.ServiceTemplateUpdateResult{Name: "agent/one", Changed: true})
	}))
	defer server.Close()

	client := New(server.URL)
	result, err := client.ServiceUpdateFromTemplate(context.Background(), "agent/one", api.ServiceUpdateTemplateRequest{
		Inputs: map[string]string{"mode": "oauth"}, DryRun: true, Overwrite: true,
	})
	if err != nil {
		t.Fatalf("ServiceUpdateFromTemplate: %v", err)
	}
	if result.Name != "agent/one" || !result.Changed {
		t.Fatalf("result = %#v", result)
	}
}
