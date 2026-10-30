package operator

import (
	"io"
	"net/http"
	"slices"

	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/gorilla/websocket"
)

// gorillaWebsocket accepts GraphQL websocket connections with gorilla's
// upgrader. gqlgen v0.17.94 made its websocket library pluggable and defaults
// to coder/websocket; keeping gorilla keeps the upgrader's CheckOrigin, the
// operator's cross-site WebSocket hijacking guard, as it was, and the library
// the log stream already upgrades with.
type gorillaWebsocket struct {
	upgrader websocket.Upgrader
}

var _ transport.WebsocketImplementation = gorillaWebsocket{}

// Accept upgrades the request, offering gqlgen's GraphQL subprotocols beside
// any the upgrader names. A rejected origin is answered 403 by gorilla itself.
func (g gorillaWebsocket) Accept(w http.ResponseWriter, r *http.Request, options transport.WebsocketAcceptOptions) (transport.WebsocketConn, error) {
	upgrader := g.upgrader
	upgrader.Subprotocols = slices.Clone(upgrader.Subprotocols)
	for _, subprotocol := range options.Subprotocols {
		if !slices.Contains(upgrader.Subprotocols, subprotocol) {
			upgrader.Subprotocols = append(upgrader.Subprotocols, subprotocol)
		}
	}
	conn, err := upgrader.Upgrade(w, r, options.ResponseHeader)
	if err != nil {
		return nil, err
	}
	return gorillaConn{conn}, nil
}

// gorillaConn adapts a gorilla connection to gqlgen's websocket transport. The
// embedded connection supplies Close, WriteJSON, Subprotocol, and the optional
// SetReadLimit and SetReadDeadline the transport uses for its read limit and
// pong deadline.
type gorillaConn struct {
	*websocket.Conn
}

// NextReader reports the peer's normal close as transport.ErrWebsocketClosed,
// the close codes gqlgen treated as normal when it used gorilla itself.
func (c gorillaConn) NextReader() (int, io.Reader, error) {
	messageType, r, err := c.Conn.NextReader()
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
		return messageType, nil, transport.ErrWebsocketClosed
	}
	return messageType, r, err
}

// WriteClose sends a close frame carrying code and message.
func (c gorillaConn) WriteClose(code int, message string) error {
	return c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, message))
}
