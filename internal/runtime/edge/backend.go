package edge

import (
	"fmt"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
)

// Backend mutates the compiled compose at compile time: it injects the edge
// service and network, drops routed services' host ports, and stamps router
// labels. It is pure compile-time behavior and makes no runtime calls.
type Backend interface {
	Contribute(stack *manifest.Stack, compiled *compose.File) error
}

// NoneBackend leaves the compiled compose unchanged when ingress is disabled.
type NoneBackend struct{}

// Contribute is a no-op for NoneBackend.
func (NoneBackend) Contribute(stack *manifest.Stack, compiled *compose.File) error {
	return nil
}

// FromManifest returns the ingress backend selected by the manifest config.
func FromManifest(cfg manifest.Ingress) (Backend, error) {
	switch cfg.Type {
	case "", "none":
		return NoneBackend{}, nil
	case "caddy":
		return NewCaddyBackend(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported ingress backend %q", cfg.Type)
	}
}
