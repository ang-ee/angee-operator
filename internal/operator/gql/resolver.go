package gql

import (
	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

// TokenMinter mints scoped connection tokens. Implemented by the operator
// package's tokenMinter; injected here as an interface so the gql package
// stays free of crypto/jwt deps.
type TokenMinter interface {
	Mint(actor, ttl string) (api.ConnectionTokenResponse, error)
}

type Resolver struct {
	Platform *service.Platform
	Events   *EventHub
	Tokens   TokenMinter
}
