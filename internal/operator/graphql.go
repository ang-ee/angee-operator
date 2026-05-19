package operator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/fyltr/angee/api"
	opgql "github.com/fyltr/angee/internal/operator/gql"
	"github.com/fyltr/angee/internal/service"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

const maxGraphQLBodyBytes = 1 << 20

var errUnsupportedGraphQLMediaType = errors.New("unsupported GraphQL content type")

func newGraphQLHandler(s *Server) (http.Handler, error) {
	gqlServer := handler.New(opgql.NewExecutableSchema(opgql.Config{
		Resolvers: &opgql.Resolver{Platform: s.platform, Events: s.eventHub},
	}))
	// SSE must be registered before POST so the Accept-based dispatch picks
	// it up for `text/event-stream` requests (see gqlgen issue #3275).
	gqlServer.AddTransport(transport.SSE{})
	gqlServer.AddTransport(transport.POST{})
	gqlServer.AddTransport(transport.GRAPHQL{})
	gqlServer.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	gqlServer.Use(extension.Introspection{})
	gqlServer.SetErrorPresenter(formatGraphQLError)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}), nil
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
