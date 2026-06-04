package service

import (
	"context"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

func (p *Platform) ServiceEndpoint(ctx context.Context, name string) (*api.ServiceEndpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	service, exists := stack.Services[name]
	if !exists {
		return nil, &NotFoundError{Kind: "service", Name: name}
	}

	endpoint := &api.ServiceEndpoint{
		Routed:       isRouted(stack, service),
		InternalHost: name,
	}
	if service.Route != nil {
		endpoint.InternalPort = service.Route.Port
	}
	if endpoint.Routed {
		endpoint.URL = routeURL(name, service.Route, ingressDomain(stack))
	}
	return endpoint, nil
}

func (p *Platform) IngressStatus(ctx context.Context) (*api.IngressStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}

	status := &api.IngressStatus{
		Type:   stack.Ingress.Type,
		Routes: []api.RouteRef{},
	}
	if stack.Ingress.Type != "caddy" {
		return status, nil
	}

	domain := ingressDomain(stack)
	status.Domain = domain
	for _, name := range sortedKeys(stack.Services) {
		service := stack.Services[name]
		if service.Route == nil {
			continue
		}
		status.Routes = append(status.Routes, api.RouteRef{
			Service: name,
			URL:     routeURL(name, service.Route, domain),
		})
	}
	return status, nil
}

func ingressDomain(stack *manifest.Stack) string {
	if stack.Ingress.Domain != "" {
		return stack.Ingress.Domain
	}
	return stack.Operator.Domain
}

func isRouted(stack *manifest.Stack, service manifest.Service) bool {
	return stack.Ingress.Type == "caddy" && service.Route != nil
}

// routeURL assumes routed endpoints are WebSocket agent endpoints and therefore
// uses wss:// because manifest.Route does not carry a scheme today.
func routeURL(serviceName string, route *manifest.Route, domain string) string {
	host := route.Host
	if host == "" {
		if domain != "" {
			host = serviceName + "." + domain
		} else {
			host = serviceName
		}
	}
	return "wss://" + host + "/"
}
