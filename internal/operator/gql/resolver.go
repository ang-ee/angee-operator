package gql

import (
	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
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
}
