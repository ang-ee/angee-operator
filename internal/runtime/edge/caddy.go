package edge

import (
	"fmt"
	"sort"

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

	routing := b.cfg.RoutingMode()
	tls := b.cfg.TLSMode()

	// addr prefixes the Caddy site address. tls: off forces plain HTTP (no
	// automatic HTTPS), so the edge speaks ws:// on its host port (default 80,
	// overridable via ingress.port so parallel stacks on one host don't all
	// contend for :80 — the consumer URL carries the same port).
	addr := ""
	edgePorts := []string{"443:443", "80:80"}
	if tls == "off" {
		addr = "http://"
		edgePorts = []string{fmt.Sprintf("%d:80", b.cfg.HostPort())}
	}

	// Path mode contributes one handle_path block per routed service to a single
	// shared site. Each block's label keys must be unique across containers so
	// caddy-docker-proxy merges (rather than overwrites) them; a deterministic
	// per-service index drives caddy-docker-proxy's numeric order prefix.
	routedIndex := routedServiceIndex(stack)

	if compiled.Networks == nil {
		compiled.Networks = map[string]compose.Network{}
	}
	if _, ok := compiled.Networks[network]; !ok {
		compiled.Networks[network] = compose.Network{}
	}
	// Explicitly declare the implicit default network: services list it by
	// name (edge, routed upstreams), and the compose validator requires every
	// referenced network to be declared.
	if _, ok := compiled.Networks["default"]; !ok {
		compiled.Networks["default"] = compose.Network{}
	}

	if compiled.Services == nil {
		compiled.Services = map[string]compose.Service{}
	}
	// TODO: name-collision with a user service "edge" is out of scope.
	// TLS at the edge is handled automatically by Caddy when the host is a real
	// domain (no label needed); the spike confirmed HTTP-only works for dev.
	compiled.Services["edge"] = compose.Service{
		Image:   image,
		Ports:   edgePorts,
		Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock:ro"},
		// host.docker.internal is Docker Desktop magic; host-gateway gives
		// plain-Linux edge containers host-local forward auth and is harmless on Desktop.
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
		// The edge lives on both networks: the edge network carries routed
		// upstreams; the default network resolves the forward_auth target when
		// it is a compose service (the docker-mode operator container).
		Networks:   []string{"default", network},
	}

	for name, svc := range compiled.Services {
		manifestService, ok := stack.Services[name]
		if !ok || manifestService.Route == nil {
			continue
		}

		route := manifestService.Route
		svc.Ports = nil
		// A compose service with an explicit network list loses the implicit
		// default network — a routed service must stay on it (its backends,
		// e.g. vite -> django, resolve by compose DNS there) AND join the edge.
		if len(svc.Networks) == 0 {
			svc.Networks = []string{"default"}
		}
		svc.Networks = appendUnique(svc.Networks, network)
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}

		if routing == "path" {
			// One shared site (caddy: <domain>) with a prefix-stripping
			// handle_path per service. handle_path strips the matched prefix, so
			// the backend serving at / sees / regardless of the public prefix.
			hp := fmt.Sprintf("caddy.%d_handle_path", routedIndex[name])
			svc.Labels["caddy"] = addr + domain
			svc.Labels[hp] = route.PathPrefix(name) + "/*"
			svc.Labels[hp+".reverse_proxy"] = fmt.Sprintf("{{upstreams %d}}", route.Port)
			svc.Labels[hp+".reverse_proxy.flush_interval"] = "-1"
			if route.Auth != "none" {
				svc.Labels[hp+".forward_auth"] = verify
				svc.Labels[hp+".forward_auth.uri"] = "/edge/verify?service=" + name
			}
			compiled.Services[name] = svc
			continue
		}

		svc.Labels["caddy"] = addr + route.HostName(name, domain)
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

// routedServiceIndex assigns a stable 0-based index to each routed service.
// Non-root routes are ordered by service name before root routes, which are also
// name-sorted. Path mode uses the index as caddy-docker-proxy's directive order
// prefix; root routes must have the highest indices because their catch-all
// handle_path would otherwise shadow every later prefixed route.
func routedServiceIndex(stack *manifest.Stack) map[string]int {
	names := make([]string, 0, len(stack.Services))
	rootNames := make([]string, 0, 1)
	for name, svc := range stack.Services {
		if svc.Route != nil {
			if svc.Route.PathPrefix(name) == "" {
				rootNames = append(rootNames, name)
			} else {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	sort.Strings(rootNames)
	names = append(names, rootNames...)
	index := make(map[string]int, len(names))
	for i, name := range names {
		index[name] = i
	}
	return index
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}
