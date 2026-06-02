package edge

import (
	"fmt"

	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/runtime/compose"
)

// Backend mutates the compiled compose at compile time: it injects the edge
// service and network, drops routed services' host ports, and stamps router
// labels. It is pure compile-time behavior and makes no runtime calls.
type Backend interface {
	Contribute(stack *manifest.Stack, compiled *compose.File) error
}

type NoneBackend struct{}

func (NoneBackend) Contribute(stack *manifest.Stack, compiled *compose.File) error {
	return nil
}

func FromManifest(cfg manifest.Ingress) (Backend, error) {
	switch cfg.Type {
	case "", "none":
		return NoneBackend{}, nil
	case "caddy":
		// TODO(chunk D): return NewCaddyBackend(cfg)
		return nil, fmt.Errorf("caddy ingress backend not yet implemented")
	default:
		return nil, fmt.Errorf("unsupported ingress backend %q", cfg.Type)
	}
}
