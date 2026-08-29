package service

import (
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestCompileIngress_NoneIsInert(t *testing.T) {
	stack := &manifest.Stack{
		Name: "demo",
		Services: map[string]manifest.Service{
			"web": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
			},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	compiled, err := Compile(stack, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if _, ok := compiled.Compose.Services["edge"]; ok {
		t.Fatal(`compiled.Compose.Services["edge"] present, want absent`)
	}
	if len(compiled.Compose.Networks) != 0 {
		t.Fatalf("compiled.Compose.Networks = %#v, want empty", compiled.Compose.Networks)
	}
	web := compiled.Compose.Services["web"]
	if want := []string{"host.docker.internal:host-gateway"}; !reflect.DeepEqual(web.ExtraHosts, want) {
		t.Fatalf("web.ExtraHosts = %#v, want %#v", web.ExtraHosts, want)
	}
}

func TestCompileIngress_CaddyInjects(t *testing.T) {
	stack := &manifest.Stack{
		Name:    "demo",
		Ingress: manifest.Ingress{Type: "caddy", Domain: "agents.localhost"},
		Services: map[string]manifest.Service{
			"agent": {
				Runtime: manifest.RuntimeContainer,
				Image:   "nginx:latest",
				Ports:   manifest.StringList{"127.0.0.1:3008:3008"},
				Route:   &manifest.Route{Port: 3008},
			},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	compiled, err := Compile(stack, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	edge, ok := compiled.Compose.Services["edge"]
	if !ok {
		t.Fatal(`compiled.Compose.Services["edge"] missing`)
	}
	if want := []string{"host.docker.internal:host-gateway"}; !reflect.DeepEqual(edge.ExtraHosts, want) {
		t.Fatalf("edge.ExtraHosts = %#v, want %#v", edge.ExtraHosts, want)
	}
	agent := compiled.Compose.Services["agent"]
	if len(agent.Ports) != 0 {
		t.Fatalf("agent.Ports = %#v, want empty", agent.Ports)
	}
	if want := []string{"host.docker.internal:host-gateway"}; !reflect.DeepEqual(agent.ExtraHosts, want) {
		t.Fatalf("agent.ExtraHosts = %#v, want %#v", agent.ExtraHosts, want)
	}
}
