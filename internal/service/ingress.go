package service

import (
	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
)

func (p *Platform) ServiceEndpoint(name string) (*api.ServiceEndpoint, error) {
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

func (p *Platform) IngressStatus() (*api.IngressStatus, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}

	domain := ingressDomain(stack)
	status := &api.IngressStatus{
		Type:   stack.Ingress.Type,
		Domain: domain,
		Routes: []api.RouteRef{},
	}
	if stack.Ingress.Type != "caddy" {
		return status, nil
	}

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
