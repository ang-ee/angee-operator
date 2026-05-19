package gql

import "github.com/fyltr/angee/internal/service"

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Platform *service.Platform
	Events   *EventHub
}
