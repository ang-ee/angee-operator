package platformclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
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
