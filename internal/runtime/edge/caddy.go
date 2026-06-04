package edge

import (
	"fmt"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
)

const (
	defaultCaddyImage = "lucaslorentz/caddy-docker-proxy:2.9"
	// defaultEdgeVerifyTarget is the forward_auth upstream (host:port) when the
	// manifest doesn't set ingress.verify; the operator must be reachable under
	// this name on the edge network.
	defaultEdgeVerifyTarget = "operator:9000"
)

// CaddyBackend contributes caddy-docker-proxy ingress services, networks, and labels.
type CaddyBackend struct {
	cfg manifest.Ingress
}

// NewCaddyBackend returns a Caddy ingress backend configured from the manifest.
func NewCaddyBackend(cfg manifest.Ingress) *CaddyBackend {
	return &CaddyBackend{cfg: cfg}
}

// Contribute mutates the compiled compose file with Caddy edge ingress wiring.
func (b *CaddyBackend) Contribute(stack *manifest.Stack, compiled *compose.File) error {
	network := b.cfg.Network
	if network == "" {
		network = stack.Name + "_edge"
	}

	image := b.cfg.Image
	if image == "" {
		image = defaultCaddyImage
	}

	domain := b.cfg.Domain
	if domain == "" {
		domain = stack.Operator.Domain
	}

	// verify is the forward_auth target (host:port reachable from the edge
	// network); the operator's /edge/verify endpoint. Validated by the Caddy
	// run-spike: a direct per-service forward_auth directive works — no global
	// snippet is needed.
	verify := b.cfg.Verify
	if verify == "" {
		verify = defaultEdgeVerifyTarget
	}

	if compiled.Networks == nil {
		compiled.Networks = map[string]compose.Network{}
	}
	if _, ok := compiled.Networks[network]; !ok {
		compiled.Networks[network] = compose.Network{}
	}

	if compiled.Services == nil {
		compiled.Services = map[string]compose.Service{}
	}
	// TODO: name-collision with a user service "edge" is out of scope.
	// TLS at the edge is handled automatically by Caddy when the host is a real
	// domain (no label needed); the spike confirmed HTTP-only works for dev.
	compiled.Services["edge"] = compose.Service{
		Image:    image,
		Ports:    []string{"443:443", "80:80"},
		Volumes:  []string{"/var/run/docker.sock:/var/run/docker.sock:ro"},
		Networks: []string{network},
	}

	for name, svc := range compiled.Services {
		manifestService, ok := stack.Services[name]
		if !ok || manifestService.Route == nil {
			continue
		}

		route := manifestService.Route
		svc.Ports = nil
		svc.Networks = appendUnique(svc.Networks, network)
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}

		host := route.Host
		if host == "" {
			if domain != "" {
				host = name + "." + domain
			} else {
				host = name
			}
		}

		svc.Labels["caddy"] = host
		svc.Labels["caddy.reverse_proxy"] = fmt.Sprintf("{{upstreams %d}}", route.Port)
		svc.Labels["caddy.reverse_proxy.flush_interval"] = "-1"
		if route.Auth != "none" {
			// Per-service forward_auth to the operator's /edge/verify. The
			// client token rides ?token= and reaches /edge/verify via the
			// X-Forwarded-Uri header that forward_auth sets (spike-validated).
			svc.Labels["caddy.forward_auth"] = verify
			svc.Labels["caddy.forward_auth.uri"] = "/edge/verify?service=" + name
		}

		compiled.Services[name] = svc
	}

	return nil
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}
