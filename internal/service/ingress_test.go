package service

import (
	"errors"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
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

	endpoint, err := platform.ServiceEndpoint("agent")
	if err != nil {
		t.Fatalf("ServiceEndpoint() error = %v", err)
	}
	if endpoint.Routed {
		t.Fatalf("ServiceEndpoint().Routed = true, want false")
	}
	if endpoint.URL != "" {
		t.Fatalf("ServiceEndpoint().URL = %q, want empty", endpoint.URL)
	}

	status, err := platform.IngressStatus()
	if err != nil {
		t.Fatalf("IngressStatus() error = %v", err)
	}
	if status.Type != "none" {
		t.Fatalf("IngressStatus().Type = %q, want none", status.Type)
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

	endpoint, err := platform.ServiceEndpoint("agent")
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

	status, err := platform.IngressStatus()
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

	_, err := platform.ServiceEndpoint("missing")
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
