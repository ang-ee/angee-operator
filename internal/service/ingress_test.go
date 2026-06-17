package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestServiceEndpointIngressNone(t *testing.T) {
	platform := newIngressTestPlatform(t, &manifest.Stack{
		Name: "demo",
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
			},
		},
	})

	endpoint, err := platform.ServiceEndpoint(context.Background(), "agent")
	if err != nil {
		t.Fatalf("ServiceEndpoint() error = %v", err)
	}
	if endpoint.Routed {
		t.Fatalf("ServiceEndpoint().Routed = true, want false")
	}
	if endpoint.URL != "" {
		t.Fatalf("ServiceEndpoint().URL = %q, want empty", endpoint.URL)
	}

	status, err := platform.IngressStatus(context.Background())
	if err != nil {
		t.Fatalf("IngressStatus() error = %v", err)
	}
	if status.Type != "none" {
		t.Fatalf("IngressStatus().Type = %q, want none", status.Type)
	}
	if status.Domain != "" {
		t.Fatalf("IngressStatus().Domain = %q, want empty", status.Domain)
	}
	if len(status.Routes) != 0 {
		t.Fatalf("IngressStatus().Routes = %#v, want empty", status.Routes)
	}
}

func TestServiceEndpointIngressCaddy(t *testing.T) {
	platform := newIngressTestPlatform(t, &manifest.Stack{
		Name:    "demo",
		Ingress: manifest.Ingress{Type: "caddy", Domain: "agents.localhost"},
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
				Route:   &manifest.Route{Port: 3008},
			},
		},
	})

	endpoint, err := platform.ServiceEndpoint(context.Background(), "agent")
	if err != nil {
		t.Fatalf("ServiceEndpoint() error = %v", err)
	}
	if !endpoint.Routed {
		t.Fatalf("ServiceEndpoint().Routed = false, want true")
	}
	if endpoint.URL != "wss://agent.agents.localhost/" {
		t.Fatalf("ServiceEndpoint().URL = %q, want wss://agent.agents.localhost/", endpoint.URL)
	}
	if endpoint.InternalPort != 3008 {
		t.Fatalf("ServiceEndpoint().InternalPort = %d, want 3008", endpoint.InternalPort)
	}

	status, err := platform.IngressStatus(context.Background())
	if err != nil {
		t.Fatalf("IngressStatus() error = %v", err)
	}
	if len(status.Routes) != 1 {
		t.Fatalf("IngressStatus().Routes = %#v, want one route", status.Routes)
	}
	if status.Routes[0].Service != "agent" || status.Routes[0].URL != "wss://agent.agents.localhost/" {
		t.Fatalf("IngressStatus().Routes[0] = %#v, want agent route", status.Routes[0])
	}
}

func TestServiceEndpointIngressPath(t *testing.T) {
	platform := newIngressTestPlatform(t, &manifest.Stack{
		Name:    "demo",
		Ingress: manifest.Ingress{Type: "caddy", Routing: "path", TLS: "off", Domain: "localhost"},
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
				Route:   &manifest.Route{Port: 3008},
			},
		},
	})

	endpoint, err := platform.ServiceEndpoint(context.Background(), "agent")
	if err != nil {
		t.Fatalf("ServiceEndpoint() error = %v", err)
	}
	if !endpoint.Routed {
		t.Fatalf("ServiceEndpoint().Routed = false, want true")
	}
	if endpoint.URL != "ws://localhost/agent/" {
		t.Fatalf("ServiceEndpoint().URL = %q, want ws://localhost/agent/", endpoint.URL)
	}

	status, err := platform.IngressStatus(context.Background())
	if err != nil {
		t.Fatalf("IngressStatus() error = %v", err)
	}
	if len(status.Routes) != 1 {
		t.Fatalf("IngressStatus().Routes = %#v, want one route", status.Routes)
	}
	if status.Routes[0].Service != "agent" || status.Routes[0].URL != "ws://localhost/agent/" {
		t.Fatalf("IngressStatus().Routes[0] = %#v, want agent path route", status.Routes[0])
	}
}

func TestRouteURL(t *testing.T) {
	tests := []struct {
		name    string
		ingress manifest.Ingress
		service string
		route   *manifest.Route
		domain  string
		want    string
	}{
		{
			name:    "host default subdomain",
			ingress: manifest.Ingress{Type: "caddy"},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "agents.localhost",
			want:    "wss://agent.agents.localhost/",
		},
		{
			name:    "host no domain",
			ingress: manifest.Ingress{Type: "caddy"},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "",
			want:    "wss://agent/",
		},
		{
			name:    "host custom",
			ingress: manifest.Ingress{Type: "caddy"},
			service: "agent",
			route:   &manifest.Route{Port: 3008, Host: "custom.example.com"},
			domain:  "agents.localhost",
			want:    "wss://custom.example.com/",
		},
		{
			name:    "host custom subdomain with custom edge port",
			ingress: manifest.Ingress{Type: "caddy", TLS: "off", Port: 7003},
			service: "agent",
			route:   &manifest.Route{Port: 3008, Host: "custom.example.com"},
			domain:  "agents.localhost",
			want:    "ws://custom.example.com:7003/",
		},
		{
			name:    "path default prefix",
			ingress: manifest.Ingress{Type: "caddy", Routing: "path"},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "notes.localhost",
			want:    "wss://notes.localhost/agent/",
		},
		{
			name:    "path custom prefix",
			ingress: manifest.Ingress{Type: "caddy", Routing: "path"},
			service: "agent",
			route:   &manifest.Route{Port: 3008, Path: "custom"},
			domain:  "notes.localhost",
			want:    "wss://notes.localhost/custom/",
		},
		{
			name:    "path tls off",
			ingress: manifest.Ingress{Type: "caddy", Routing: "path", TLS: "off"},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "localhost",
			want:    "ws://localhost/agent/",
		},
		{
			name:    "host tls off",
			ingress: manifest.Ingress{Type: "caddy", TLS: "off"},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "agents.localhost",
			want:    "ws://agent.agents.localhost/",
		},
		{
			name:    "path tls off custom edge port",
			ingress: manifest.Ingress{Type: "caddy", Routing: "path", TLS: "off", Port: 7003},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "localhost",
			want:    "ws://localhost:7003/agent/",
		},
		{
			name:    "host tls off custom edge port",
			ingress: manifest.Ingress{Type: "caddy", TLS: "off", Port: 7003},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "agents.localhost",
			want:    "ws://agent.agents.localhost:7003/",
		},
		{
			name:    "explicit default port 80 stays bare",
			ingress: manifest.Ingress{Type: "caddy", Routing: "path", TLS: "off", Port: 80},
			service: "agent",
			route:   &manifest.Route{Port: 3008},
			domain:  "localhost",
			want:    "ws://localhost/agent/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeURL(tt.ingress, tt.service, tt.route, tt.domain); got != tt.want {
				t.Fatalf("routeURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServiceEndpointMissing(t *testing.T) {
	platform := newIngressTestPlatform(t, &manifest.Stack{
		Name: "demo",
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
			},
		},
	})

	_, err := platform.ServiceEndpoint(context.Background(), "missing")
	if err == nil {
		t.Fatal("ServiceEndpoint(missing) error = nil, want error")
	}
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("ServiceEndpoint(missing) error = %v, want NotFoundError", err)
	}
}

func newIngressTestPlatform(t *testing.T, stack *manifest.Stack) *Platform {
	t.Helper()
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return platform
}
