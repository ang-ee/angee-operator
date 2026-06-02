package operator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fyltr/angee/api"
	opgql "github.com/fyltr/angee/internal/operator/gql"
	"github.com/fyltr/angee/internal/service"
	"github.com/gorilla/websocket"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const maxGraphQLBodyBytes = 1 << 20

const (
	// wsKeepAlivePingInterval keeps idle subscription sockets alive across
	// intermediary timeouts and lets dead peers be reaped.
	wsKeepAlivePingInterval = 10 * time.Second
	// wsInitTimeout bounds how long a client may hold an upgraded socket open
	// without sending connection_init.
	wsInitTimeout = 10 * time.Second
)

var (
	errUnsupportedGraphQLMediaType = errors.New("unsupported GraphQL content type")
	// errWSUnauthorized is surfaced to the client as a graphql-ws
	// connection_error and a socket close; it deliberately carries no detail.
	errWSUnauthorized = errors.New("unauthorized")
)

// newGraphQLHandler builds the shared gqlgen server and returns two HTTP
// handlers over it: post serves queries/mutations/SSE-subscriptions over
// POST (with a body cap and content-type validation), and ws serves the
// graphql-transport-ws WebSocket upgrade on GET. They share one schema,
// resolver set, and event hub; only the transport differs.
func newGraphQLHandler(s *Server) (post, ws http.Handler, err error) {
	gqlServer := handler.New(opgql.NewExecutableSchema(opgql.Config{
		Resolvers: &opgql.Resolver{Platform: s.platform, Events: s.eventHub, Tokens: s.tokens},
	}))
	// SSE must be registered before POST so the Accept-based dispatch picks
	// it up for `text/event-stream` requests (see gqlgen issue #3275).
	gqlServer.AddTransport(transport.SSE{})
	gqlServer.AddTransport(transport.POST{})
	gqlServer.AddTransport(transport.GRAPHQL{})
	// WebSocket subscriptions for browser clients. A WS upgrade carries no
	// Authorization header, so auth runs in InitFunc (the same two-tier check
	// as s.auth, reading the token from connection_init). CrossOriginProtection
	// treats the GET upgrade as safe and does not gate it, so the origin
	// allowlist on the upgrader is the cross-site-WebSocket-hijacking guard.
	// On a rejected origin gorilla writes a 403 itself; gqlgen then logs a
	// (harmless) "superfluous WriteHeader" — the client still sees the 403.
	gqlServer.AddTransport(transport.Websocket{
		Upgrader:              websocket.Upgrader{CheckOrigin: s.checkWebSocketOrigin},
		InitFunc:              s.graphqlWSInit,
		InitTimeout:           wsInitTimeout,
		KeepAlivePingInterval: wsKeepAlivePingInterval,
	})
	gqlServer.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	gqlServer.Use(extension.Introspection{})
	gqlServer.SetErrorPresenter(formatGraphQLError)

	post = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, api.ErrorResponse{Error: "graphql requires POST"})
			return
		}
		if err := validateGraphQLContentType(r); err != nil {
			writeJSON(w, http.StatusUnsupportedMediaType, api.ErrorResponse{Error: err.Error()})
			return
		}
		body, err := readGraphQLBody(w, r)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, api.ErrorResponse{Error: "request body too large"})
				return
			}
			writeBadRequest(w, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		gqlServer.ServeHTTP(w, r)
	})
	// The gqlgen server dispatches the GET upgrade to the Websocket transport;
	// the POST-only body cap and content-type checks above must not gate it.
	ws = gqlServer
	return post, ws, nil
}

// graphqlWSInit authenticates a graphql-transport-ws connection_init. The
// browser cannot set an Authorization header on a WS upgrade, so the token
// rides connection_init's payload; this runs the same two-tier check as
// s.auth and binds the resolved actor/scope to the connection context.
func (s *Server) graphqlWSInit(ctx context.Context, initPayload transport.InitPayload) (context.Context, *transport.InitPayload, error) {
	if s.config.Token == "" {
		return ctx, nil, nil
	}
	// initPayload.Authorization() reads the "Authorization"/"authorization"
	// key from connection_init; require the same "Bearer " scheme as the HTTP
	// path so the two entrypoints accept exactly the same credentials.
	token, ok := parseBearer(initPayload.Authorization())
	if !ok {
		return ctx, nil, errWSUnauthorized
	}
	claims, ok := s.authenticateBearer(token)
	if !ok {
		return ctx, nil, errWSUnauthorized
	}
	if claims != nil {
		ctx = withActorScope(ctx, *claims)
	}
	return ctx, nil, nil
}

func validateGraphQLContentType(r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return errUnsupportedGraphQLMediaType
	}
	switch mediaType {
	case "application/json", "application/graphql":
		return nil
	default:
		return errUnsupportedGraphQLMediaType
	}
}

func readGraphQLBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxGraphQLBodyBytes)
	defer limited.Close()
	return io.ReadAll(limited)
}

func formatGraphQLError(ctx context.Context, err error) *gqlerror.Error {
	gqlErr := graphql.DefaultErrorPresenter(ctx, err)
	if gqlErr.Extensions == nil {
		gqlErr.Extensions = map[string]any{}
	}

	var notFound *service.NotFoundError
	if errors.As(err, &notFound) {
		gqlErr.Extensions["kind"] = notFound.Kind
		gqlErr.Extensions["name"] = notFound.Name
		return gqlErr
	}

	var conflict *service.ConflictError
	if errors.As(err, &conflict) {
		gqlErr.Extensions["kind"] = conflict.Kind
		gqlErr.Extensions["name"] = conflict.Name
		gqlErr.Extensions["reason"] = conflict.Reason
		return gqlErr
	}

	var invalid *service.InvalidInputError
	if errors.As(err, &invalid) {
		gqlErr.Extensions["field"] = invalid.Field
		gqlErr.Extensions["reason"] = invalid.Reason
		return gqlErr
	}

	if len(gqlErr.Extensions) == 0 {
		gqlErr.Extensions = nil
	}
	return gqlErr
}
