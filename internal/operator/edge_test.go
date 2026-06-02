package operator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEdgeVerify(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  agent-x:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "admin-bearer", JWTSecret: "edge-jwt-secret-123456"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	routeToken, err := server.tokens.MintRoute("actor", "agent-x", "60s")
	if err != nil {
		t.Fatalf("MintRoute() error = %v", err)
	}
	operatorToken, err := server.tokens.MintConnection("actor", nil, "1h")
	if err != nil {
		t.Fatalf("MintConnection() error = %v", err)
	}

	tests := []struct {
		name string
		req  func() *http.Request
		want int
	}{
		{
			name: "query route token",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/edge/verify?service=agent-x&token="+routeToken.Token, nil)
			},
			want: http.StatusOK,
		},
		{
			name: "wrong service",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/edge/verify?service=other&token="+routeToken.Token, nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "operator audience token",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/edge/verify?service=agent-x&token="+operatorToken.Token, nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "missing token",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/edge/verify?service=agent-x", nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "missing service",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/edge/verify?token="+routeToken.Token, nil)
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "authorization bearer route token",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/edge/verify?service=agent-x", nil)
				req.Header.Set("Authorization", "Bearer "+routeToken.Token)
				return req
			},
			want: http.StatusOK,
		},
		{
			name: "sec websocket protocol route token",
			req: func() *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/edge/verify?service=agent-x", nil)
				req.Header.Set("Sec-WebSocket-Protocol", routeToken.Token)
				return req
			},
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rr, tt.req())
			if rr.Code == http.StatusSwitchingProtocols {
				t.Fatalf("edge verify status = %d, must never be 101", rr.Code)
			}
			if rr.Code != tt.want {
				t.Fatalf("edge verify status = %d, want %d, body = %s", rr.Code, tt.want, rr.Body.String())
			}
		})
	}
}
