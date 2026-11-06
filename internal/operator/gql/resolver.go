package gql

import (
	"context"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

// TokenMinter mints scoped connection and route tokens. Implemented by the
// operator package's tokenMinter; injected here as an interface so the gql
// package stays free of crypto/jwt deps and does not construct audiences.
type TokenMinter interface {
	// MintConnection issues an operator-API token (aud=operator) carrying scope.
	MintConnection(actor string, scope []string, ttl string) (api.ConnectionTokenResponse, error)
	// MintRoute issues a route token (aud=svc:<service>) for one service.
	MintRoute(actor, service, ttl string) (api.ConnectionTokenResponse, error)
}

type Resolver struct {
	Platform service.API
	Events   *EventHub
	Tokens   TokenMinter
	// LogStreamDescriptor builds a service's live-log-socket descriptor for the
	// serviceEndpoint resolver. Injected by the operator package (it needs the
	// request scheme/host and the token minter, which the gql package can't
	// reach); nil in contexts that don't serve it, in which case the
	// serviceEndpoint.logStream field is null.
	LogStreamDescriptor func(ctx context.Context, service string) *api.LogStream
}
