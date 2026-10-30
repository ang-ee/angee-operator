package platformclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
)

func TestSourceCheckoutClientHoldsResponseForCallbackAndReleasesOnError(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if r.Method != http.MethodPost || r.URL.Path != "/source-checkouts" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var request api.SourceCheckoutRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Repository != "ssh://repository" || request.Revision != "main" {
			t.Errorf("request = %#v, %v", request, err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(api.SourceCheckout{Path: "run/source-checkouts/test/repository", Revision: "commit"})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := New(server.URL)
	client.client.Timeout = time.Nanosecond // Scoped responses use the existing unbounded streaming client.
	failure := errors.New("metadata failure")
	err := client.WithSourceCheckout(t.Context(), api.SourceCheckoutRequest{Repository: "ssh://repository", Revision: "main"}, func(ctx context.Context, checkout api.SourceCheckout) error {
		if ctx.Err() != nil || checkout.Path != "run/source-checkouts/test/repository" {
			t.Fatalf("callback = %#v, %v", checkout, ctx.Err())
		}
		select {
		case <-done:
			t.Fatal("response released too soon")
		default:
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("response not closed")
	}
}

func TestSourceCheckoutClientCancelsCallbackWhenOperatorEndsResponse(t *testing.T) {
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.SourceCheckout{Path: "run/source-checkouts/test/repository", Revision: "commit"})
		w.(http.Flusher).Flush()
		<-finish
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := New(server.URL).WithSourceCheckout(ctx, api.SourceCheckoutRequest{Repository: "repo", Revision: "main"}, func(scope context.Context, _ api.SourceCheckout) error {
		close(finish)
		<-scope.Done()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ended operator = %v", err)
	}
}
