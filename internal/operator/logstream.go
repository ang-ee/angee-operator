package operator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/gorilla/websocket"
)

// errLogBackendNotConfigured is returned by the production LogStreamer stub
// until a durable backend is wired up. It surfaces to the client as a plain
// HTTP error before the WebSocket upgrade.
var errLogBackendNotConfigured = errors.New("production log backend is not configured")

// LogStreamer is the per-service log routing seam. The operator picks an
// implementation per service/environment; the WebSocket handler is agnostic to
// which one it streams from.
type LogStreamer interface {
	StreamService(ctx context.Context, service string, tail int) (<-chan api.LogLine, error)
}

// ephemeralStreamer is the development backend: it proxies the platform's live
// per-service follow stream with no persistence. The upstream `--follow` runs
// only while a client is connected.
type ephemeralStreamer struct {
	platform service.API
}

func (e ephemeralStreamer) StreamService(ctx context.Context, service string, tail int) (<-chan api.LogLine, error) {
	return e.platform.StreamServiceLogs(ctx, service, tail)
}

// prodStreamer is the production backend stub. A real implementation would tail
// the configured durable store (e.g. VictoriaLogs / an OTLP-fed store) filtered
// to the service; until then it fails closed so the seam is observable.
type prodStreamer struct{}

func (prodStreamer) StreamService(context.Context, string, int) (<-chan api.LogLine, error) {
	return nil, errLogBackendNotConfigured
}

// newLogStreamer selects the per-service log streaming backend from config.
// The empty string and "ephemeral" both pick the dev live-proxy; any other
// value is rejected at startup, so a misconfigured `--log-backend` fails fast
// instead of silently defaulting. Only the ephemeral backend is wired today;
// a durable production backend (the `prodStreamer` seam) adds a new case here.
func newLogStreamer(backend string, platform service.API) (LogStreamer, error) {
	switch backend {
	case "", "ephemeral":
		return ephemeralStreamer{platform: platform}, nil
	default:
		return nil, fmt.Errorf("unknown log backend %q (supported: ephemeral)", backend)
	}
}

const logSocketWriteWait = 10 * time.Second

// serviceLogsStream serves a service's live log socket over a WebSocket. Auth
// runs in-handler (the upgrade carries no Authorization header), mirroring the
// graphql-ws path: a per-service route token or the admin/operator tier. The
// stream is opened before the upgrade so a backend error (e.g. unknown service,
// or the production stub) surfaces as a clean HTTP error rather than a socket
// close. Cancelling the request context — including the client closing the
// socket — tears down the upstream follow.
func (s *Server) serviceLogsStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "service name is required"})
		return
	}
	if !s.authorizeServiceSocket(r, name) {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	lines, err := s.logStreamer.StreamService(ctx, name, logTailParam(r))
	if err != nil {
		writeError(w, err)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: s.checkWebSocketOrigin}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader has already written the failure response
	}
	defer func() { _ = conn.Close() }()

	// Reader pump: a browser sends no application messages, so any read error
	// means the client went away — cancel to tear down the upstream follow.
	// Liveness detection is deliberately write-side only: there is no read
	// deadline (it would kill idle-but-healthy sockets); a dead peer is reaped
	// when the next keepalive ping fails to write.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	ping := time.NewTicker(wsKeepAlivePingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(logSocketWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case frame, ok := <-lines:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(logSocketWriteWait))
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
		}
	}
}

// maxLogTail caps the requested backlog so a client can't ask the backend to
// replay an unbounded number of lines before the live follow begins.
const maxLogTail = 10000

// logTailParam reads the `tail` (or `n` alias) query parameter — the number of
// trailing lines to replay before following live. It clamps to [0, maxLogTail];
// a missing, non-numeric, or negative value means no backlog cap (0).
func logTailParam(r *http.Request) int {
	raw := r.URL.Query().Get("tail")
	if raw == "" {
		raw = r.URL.Query().Get("n")
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	if n > maxLogTail {
		return maxLogTail
	}
	return n
}

// authorizeServiceSocket gates the per-service log socket. With no configured
// token the operator is open (loopback dev). Otherwise the presented token must
// be a route token for this service (aud=svc:<name>) or pass the admin/operator
// tier — the same credentials and extraction the edge uses.
func (s *Server) authorizeServiceSocket(r *http.Request, service string) bool {
	if s.config.Token == "" {
		return true
	}
	raw := edgeToken(r)
	if raw == "" {
		return false
	}
	if _, err := s.tokens.Verify(raw, serviceAudience(service)); err == nil {
		return true
	}
	_, ok := s.authenticateBearer(raw)
	return ok
}

// logSocketScheme returns the WebSocket scheme matching how the client reached
// the operator (wss behind TLS, ws otherwise).
func logSocketScheme(r *http.Request) string {
	if r.TLS != nil {
		return "wss"
	}
	return "ws"
}

// logStreamDescriptor builds the LogStream the service-info endpoint returns:
// the resolved socket URL (from the scheme/host the client used to reach the
// operator) and a freshly-minted route token. Shared by the REST handler and
// the GraphQL resolver, which supply scheme/host differently. Today it always
// resolves the operator target; edge and production targets are selected here
// once configured. The context carries the actor scope used for minting.
func (s *Server) logStreamDescriptor(ctx context.Context, scheme, host, service string) *api.LogStream {
	socket := url.URL{Scheme: scheme, Host: host, Path: "/services/" + service + "/logs/stream"}
	descriptor := &api.LogStream{
		URL:      socket.String(),
		Target:   "operator",
		Protocol: "ws",
	}
	// No configured token means open dev — no credential to mint.
	if s.config.Token == "" {
		return descriptor
	}
	actor := "log-stream"
	if scope, ok := actorScopeFromContext(ctx); ok && scope.Actor != "" {
		actor = scope.Actor
	}
	token, err := s.tokens.MintRoute(actor, service, "")
	if err != nil {
		// A descriptor without a token is still useful in dev; surface the
		// minting failure only by omitting the credential.
		return descriptor
	}
	descriptor.Token = &token.Token
	descriptor.ExpiresAt = &token.ExpiresAt
	return descriptor
}
