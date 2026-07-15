package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/gorilla/websocket"
)

type fakeLogStreamer struct {
	frames  []api.LogLine
	err     error
	follow  bool // when true, block until ctx is done after emitting frames
	gotTail *int // when non-nil, records the tail value the handler passed
}

func (f fakeLogStreamer) StreamService(ctx context.Context, _ string, tail int) (<-chan api.LogLine, error) {
	if f.gotTail != nil {
		*f.gotTail = tail
	}
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan api.LogLine)
	go func() {
		defer close(ch)
		for _, fr := range f.frames {
			select {
			case ch <- fr:
			case <-ctx.Done():
				return
			}
		}
		if f.follow {
			<-ctx.Done()
		}
	}()
	return ch, nil
}

func newLogStreamTestServer(t *testing.T, token string, streamer LogStreamer) (*httptest.Server, *tokenMinter) {
	t.Helper()
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: token, JWTSecret: "log-stream-jwt-secret-123456"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.logStreamer = streamer
	ts := httptest.NewServer(server.server.Handler)
	t.Cleanup(ts.Close)
	return ts, server.tokens
}

func socketURL(ts *httptest.Server, service, token string) string {
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/services/" + service + "/logs/stream"
	if token != "" {
		u += "?token=" + url.QueryEscape(token)
	}
	return u
}

func TestServiceLogsSocketStreamsFrames(t *testing.T) {
	want := []api.LogLine{
		{Service: "web", Runtime: "container", Message: "listening on :8080"},
		{Service: "web", Runtime: "container", Message: "GET /health 200"},
	}
	ts, _ := newLogStreamTestServer(t, "", fakeLogStreamer{frames: want})

	conn, resp, err := websocket.DefaultDialer.Dial(socketURL(ts, "web", ""), nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	for i, w := range want {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var got api.LogLine
		if err := conn.ReadJSON(&got); err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		if got.Service != w.Service || got.Message != w.Message || got.Runtime != w.Runtime {
			t.Fatalf("frame %d = %#v, want %#v", i, got, w)
		}
	}
	// Stream ended → server closes the socket.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected socket close after stream end, got another message")
	}
}

func TestServiceLogsSocketAuth(t *testing.T) {
	const adminBearer = "log-socket-admin-bearer"
	ts, minter := newLogStreamTestServer(t, adminBearer, fakeLogStreamer{frames: []api.LogLine{{Service: "web", Message: "hi"}}})

	// No token → rejected before upgrade.
	_, resp, err := websocket.DefaultDialer.Dial(socketURL(ts, "web", ""), nil)
	if err == nil {
		t.Fatal("expected unauthenticated dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth dial status = %v, want 401", resp)
	}

	// Route token for the WRONG service → rejected.
	wrong, err := minter.MintRoute("tester", "db", "1h")
	if err != nil {
		t.Fatalf("MintRoute(db): %v", err)
	}
	_, resp, err = websocket.DefaultDialer.Dial(socketURL(ts, "web", wrong.Token), nil)
	if err == nil {
		t.Fatal("expected wrong-service token dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-service dial status = %v, want 401", resp)
	}

	// Correct route token → accepted.
	right, err := minter.MintRoute("tester", "web", "1h")
	if err != nil {
		t.Fatalf("MintRoute(web): %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(socketURL(ts, "web", right.Token), nil)
	if err != nil {
		t.Fatalf("authorized dial failed: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got api.LogLine
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read after authorized dial: %v", err)
	}
	if got.Message != "hi" {
		t.Fatalf("got %#v, want message hi", got)
	}
}

func TestServiceLogsSocketProdStubErrorsBeforeUpgrade(t *testing.T) {
	ts, _ := newLogStreamTestServer(t, "", prodStreamer{})
	_, resp, err := websocket.DefaultDialer.Dial(socketURL(ts, "web", ""), nil)
	if err == nil {
		t.Fatal("expected dial to fail when the prod backend is not configured")
	}
	if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("expected a non-101 HTTP error, got %v", resp)
	}
}

func TestNewLogStreamer(t *testing.T) {
	// Empty and "ephemeral" both select the dev live-proxy; any other value is
	// rejected so a misconfigured --log-backend fails fast at startup.
	for _, backend := range []string{"", "ephemeral"} {
		got, err := newLogStreamer(backend, nil)
		if err != nil {
			t.Fatalf("newLogStreamer(%q) error = %v", backend, err)
		}
		if _, ok := got.(ephemeralStreamer); !ok {
			t.Fatalf("newLogStreamer(%q) = %T, want ephemeralStreamer", backend, got)
		}
	}
	if _, err := newLogStreamer("victorialogs", nil); err == nil {
		t.Fatal("newLogStreamer(unknown) expected an error, got nil")
	}
}

func TestLogTailParam(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 0},
		{"tail=5", 5},
		{"n=7", 7},                  // alias
		{"tail=3&n=9", 3},           // tail wins over n
		{"tail=abc", 0},             // non-numeric
		{"tail=-4", 0},              // negative
		{"tail=999999", maxLogTail}, // clamped
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://op/services/web/logs/stream?"+c.query, nil)
		if got := logTailParam(r); got != c.want {
			t.Errorf("logTailParam(%q) = %d, want %d", c.query, got, c.want)
		}
	}
}

func TestServiceLogsSocketPassesTail(t *testing.T) {
	var tail int
	// The fake records the tail and emits one frame; reading that frame over
	// the socket establishes a happens-before edge before we read `tail`.
	ts, _ := newLogStreamTestServer(t, "", fakeLogStreamer{
		frames:  []api.LogLine{{Service: "web", Message: "hi"}},
		gotTail: &tail,
	})
	conn, _, err := websocket.DefaultDialer.Dial(socketURL(ts, "web", "")+"?tail=42", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got api.LogLine
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if tail != 42 {
		t.Fatalf("handler passed tail = %d, want 42", tail)
	}
}

func TestLogStreamDescriptor(t *testing.T) {
	minter, err := newTokenMinter("descriptor-jwt-secret-1234567", "admin-bearer")
	if err != nil {
		t.Fatalf("newTokenMinter: %v", err)
	}
	s := &Server{config: Config{Token: "admin-bearer"}, tokens: minter}
	r := httptest.NewRequest(http.MethodGet, "http://operator.example:9000/services/web/endpoint", nil)

	ls := s.logStreamDescriptor(r.Context(), logSocketScheme(r), r.Host, "web")
	if ls == nil {
		t.Fatal("nil descriptor")
		return
	}
	if ls.Target != "operator" || ls.Protocol != "ws" {
		t.Fatalf("descriptor target/protocol = %q/%q", ls.Target, ls.Protocol)
	}
	wantURL := "ws://operator.example:9000/services/web/logs/stream"
	if ls.URL != wantURL {
		t.Fatalf("descriptor URL = %q, want %q", ls.URL, wantURL)
	}
	if ls.Token == nil {
		t.Fatal("expected a minted token when a bearer is configured")
	}
	// The minted token must authorize the web service socket.
	if _, err := minter.Verify(*ls.Token, serviceAudience("web")); err != nil {
		t.Fatalf("descriptor token does not verify for svc:web: %v", err)
	}
}

// TestGraphQLServiceEndpointLogStream guards the GraphQL wiring: the field must
// resolve to a populated descriptor (the struct field is only set by the
// resolver, never by the platform), not the structural null it would be if the
// resolver returned the platform endpoint unmodified.
func TestGraphQLServiceEndpointLogStream(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\nservices:\n  api:\n    runtime: container\n    image: nginx:latest\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ serviceEndpoint(name: "api") { url logStream { url target protocol } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	endpoint, ok := resp.Data["serviceEndpoint"].(map[string]any)
	if !ok {
		t.Fatalf("serviceEndpoint = %#v", resp.Data["serviceEndpoint"])
	}
	ls, ok := endpoint["logStream"].(map[string]any)
	if !ok {
		t.Fatalf("serviceEndpoint.logStream = %#v, want a populated descriptor (not null)", endpoint["logStream"])
	}
	if ls["target"] != "operator" || ls["protocol"] != "ws" {
		t.Fatalf("logStream target/protocol = %v/%v", ls["target"], ls["protocol"])
	}
	url, _ := ls["url"].(string)
	if !strings.HasPrefix(url, "ws://") || !strings.HasSuffix(url, "/services/api/logs/stream") {
		t.Fatalf("logStream.url = %q, want ws://<host>/services/api/logs/stream", url)
	}
}
