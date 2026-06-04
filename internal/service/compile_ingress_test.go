package service

import (
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
	if _, ok := compiled.Compose.Services["edge"]; !ok {
		t.Fatal(`compiled.Compose.Services["edge"] missing`)
	}
	agent := compiled.Compose.Services["agent"]
	if len(agent.Ports) != 0 {
		t.Fatalf("agent.Ports = %#v, want empty", agent.Ports)
	}
}
