package edge

import (
	"fmt"

	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/runtime/compose"
)

const defaultCaddyImage = "lucaslorentz/caddy-docker-proxy:2.9"

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
	// TODO(spike): define the forward_auth_edge global snippet + TLS on the edge service.
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
			svc.Labels["caddy.import"] = "forward_auth_edge " + name
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
