package platformclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type notifyReadCloser struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func newPipeListener() *pipeListener {
	return &pipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

func (r *notifyReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDoJSONTracesRedactedURL(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	client := &RemoteClient{
		baseURL: "https://user:password@example.invalid",
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
	}
	if err := client.doJSON(ctx, http.MethodGet, "/trace", nil, nil, nil); err != nil {
		t.Fatalf("doJSON() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "http GET https://***@example.invalid/trace") ||
		!strings.Contains(got, "http finished status=204 duration=") {
		t.Fatalf("trace output = %q", got)
	}
	if strings.Contains(got, "user") || strings.Contains(got, "password") {
		t.Fatalf("trace output leaked URL userinfo: %q", got)
	}
}

func TestNonStreamingRequestTimeout(t *testing.T) {
	t.Setenv("ANGEE_OPERATOR_TIMEOUT", "20ms")
	listener := newPipeListener()
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})},
	}
	server.Start()
	defer server.Close()

	client := New(server.URL)
	client.client.Transport = &http.Transport{DialContext: listener.DialContext}
	if client.client.Timeout != 20*time.Millisecond {
		t.Fatalf("request client timeout = %s, want 20ms", client.client.Timeout)
	}
	if client.streamClient.Timeout != 0 {
		t.Fatalf("stream client timeout = %s, want disabled", client.streamClient.Timeout)
	}
	started := time.Now()
	err := client.doJSON(t.Context(), http.MethodGet, "/stall", nil, nil, nil)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("doJSON() took %s, want bounded failure", elapsed)
	}
	want := "operator request GET /stall timed out after 20ms (ANGEE_OPERATOR_TIMEOUT)"
	if err == nil || err.Error() != want {
		t.Fatalf("doJSON() error = %v, want %q", err, want)
	}
}

func TestStreamTraceCompletesWhenConsumerCancels(t *testing.T) {
	var logs synchronizedBuffer
	baseCtx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	ctx, cancel := context.WithCancel(baseCtx)
	bodyClosed := make(chan struct{})
	client := &RemoteClient{
		baseURL: "https://example.invalid",
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &notifyReadCloser{
					Reader: strings.NewReader("unconsumed line\n"),
					closed: bodyClosed,
				},
			}, nil
		})},
	}
	ch, err := client.stream(ctx, "/stream", nil)
	if err != nil {
		t.Fatalf("stream() error = %v", err)
	}
	cancel()
	select {
	case <-bodyClosed:
	case <-time.After(time.Second):
		t.Fatal("stream body was not closed after consumer cancellation")
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream returned an unexpected line after cancellation")
	}
	got := logs.String()
	if !strings.Contains(got, "http finished status=200 duration=") || !strings.Contains(got, "context canceled") {
		t.Fatalf("trace output = %q, want canceled completion", got)
	}
}

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

func TestTemplateInputsMethods(t *testing.T) {
	cases := []struct {
		name string
		path string
		call func(*RemoteClient) (api.TemplateInputsResponse, error)
	}{
		{"stack", "/stack/template-inputs", func(client *RemoteClient) (api.TemplateInputsResponse, error) {
			return client.StackTemplateInputs(t.Context())
		}},
		{"workspace", "/workspaces/feature%2Fone/template-inputs", func(client *RemoteClient) (api.TemplateInputsResponse, error) {
			return client.WorkspaceTemplateInputs(t.Context(), "feature/one")
		}},
		{"service", "/services/agent%2Fone/template-inputs", func(client *RemoteClient) (api.TemplateInputsResponse, error) {
			return client.ServiceTemplateInputs(t.Context(), "agent/one")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := api.TemplateInputsResponse{
				Target: tc.name,
				Template: api.TemplateDescriptor{Ref: "templates/example", Inputs: []api.TemplateInputDescriptor{
					{Name: "topic", Question: true}, {Name: "token", Question: true, Secret: true},
				}},
				Recorded: map[string]string{"topic": "recorded"}, Unrecorded: []string{"token"},
			}
			status := http.StatusOK
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.EscapedPath() != tc.path || req.URL.RawQuery != "" {
					t.Fatalf("request = %s %s, want GET %s", req.Method, req.URL, tc.path)
				}
				w.WriteHeader(status)
				if status != http.StatusOK {
					_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "template origin is missing"})
					return
				}
				_ = json.NewEncoder(w).Encode(want)
			})
			client := New("http://operator.test")
			client.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, req)
				return recorder.Result(), nil
			})
			got, err := tc.call(client)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("TemplateInputs() = %+v, %v; want %+v", got, err, want)
			}
			status = http.StatusBadRequest
			if _, err := tc.call(client); err == nil || !strings.Contains(err.Error(), "template origin is missing") {
				t.Fatalf("TemplateInputs() error = %v, want template origin error", err)
			}
		})
	}
}
