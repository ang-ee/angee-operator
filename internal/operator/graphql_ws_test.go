package operator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOriginAllowed(t *testing.T) {
	allowed := []string{"https://console.example.com", "https://app.example.com:8443"}
	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"no origin (non-browser)", "", true},
		{"loopback localhost", "http://localhost:5173", true},
		{"loopback 127.0.0.1", "http://127.0.0.1:9000", true},
		{"loopback ipv6", "http://[::1]:3000", true},
		{"allowlisted", "https://console.example.com", true},
		{"allowlisted with port, case-insensitive", "https://APP.example.com:8443", true},
		{"not allowlisted", "https://evil.example.com", false},
		{"allowlisted host but wrong port", "https://app.example.com:9999", false},
		{"unparseable", "://nonsense", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := originAllowed(tc.origin, allowed); got != tc.want {
				t.Fatalf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

// wsMessage is the graphql-transport-ws wire envelope.
type wsMessage struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func TestGraphQLWebSocketAuth(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	const adminBearer = "ws-admin-bearer-secret"
	server, err := NewServer(Config{
		Root:      root,
		Bind:      "127.0.0.1",
		Port:      9000,
		Token:     adminBearer,
		JWTSecret: "ws-jwt-secret-1234567890",
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/graphql"

	minted, err := server.tokens.MintConnection("alice", []string{"service:read"}, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}

	dial := func(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
		t.Helper()
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		d := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}, HandshakeTimeout: 5 * time.Second}
		return d.Dial(wsURL, header)
	}

	// init sends connection_init with the given Authorization payload and
	// returns the first message the server replies with.
	initAndRead := func(t *testing.T, conn *websocket.Conn, authorization string) wsMessage {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"Authorization": authorization})
		if err := conn.WriteJSON(wsMessage{Type: "connection_init", Payload: payload}); err != nil {
			t.Fatalf("write connection_init: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var msg wsMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			// A closed/errored socket on a bad token is an acceptable rejection.
			return wsMessage{Type: "<read-error: " + err.Error() + ">"}
		}
		return msg
	}

	t.Run("minted operator token is acked", func(t *testing.T) {
		conn, _, err := dial(t, "http://localhost:5173")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if msg := initAndRead(t, conn, "Bearer "+minted.Token); msg.Type != "connection_ack" {
			t.Fatalf("type = %q, want connection_ack", msg.Type)
		}
	})

	t.Run("admin bearer is acked", func(t *testing.T) {
		conn, _, err := dial(t, "http://127.0.0.1")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if msg := initAndRead(t, conn, "Bearer "+adminBearer); msg.Type != "connection_ack" {
			t.Fatalf("type = %q, want connection_ack", msg.Type)
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		conn, _, err := dial(t, "http://localhost")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if msg := initAndRead(t, conn, "Bearer not-a-jwt"); msg.Type == "connection_ack" {
			t.Fatal("invalid token got connection_ack, want rejection")
		}
	})

	t.Run("missing authorization is rejected", func(t *testing.T) {
		conn, _, err := dial(t, "http://localhost")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if msg := initAndRead(t, conn, ""); msg.Type == "connection_ack" {
			t.Fatal("empty authorization got connection_ack, want rejection")
		}
	})

	t.Run("route-audience token is rejected", func(t *testing.T) {
		routeTok, err := server.tokens.MintRoute("alice", "agent-x", "1h")
		if err != nil {
			t.Fatalf("MintRoute() error = %v", err)
		}
		conn, _, err := dial(t, "http://localhost")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if msg := initAndRead(t, conn, "Bearer "+routeTok.Token); msg.Type == "connection_ack" {
			t.Fatal("svc-audience token got connection_ack on the operator API, want rejection")
		}
	})

	t.Run("disallowed origin is rejected at the upgrade", func(t *testing.T) {
		conn, resp, err := dial(t, "https://evil.example.com")
		if err == nil {
			conn.Close()
			t.Fatal("dial from disallowed origin succeeded, want handshake rejection")
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("handshake status = %d, want 403", status)
		}
	})
}

// TestGraphQLWebSocketOpenDevMode mirrors s.auth's open path: with no
// configured token, a connection_init carrying no Authorization is acked.
func TestGraphQLWebSocketOpenDevMode(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	ts := httptest.NewServer(server.server.Handler)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/graphql"

	d := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}, HandshakeTimeout: 5 * time.Second}
	conn, _, err := d.Dial(wsURL, http.Header{"Origin": []string{"http://localhost:5173"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(wsMessage{Type: "connection_init"}); err != nil {
		t.Fatalf("write connection_init: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg wsMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read connection_ack: %v", err)
	}
	if msg.Type != "connection_ack" {
		t.Fatalf("type = %q, want connection_ack", msg.Type)
	}
}
