package edge

import (
	"reflect"
	"testing"

	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/runtime/compose"
)

func TestCaddyBackend_Contribute(t *testing.T) {
	stack := &manifest.Stack{
		Name:    "demo",
		Ingress: manifest.Ingress{Type: "caddy", Domain: "agents.localhost"},
		Services: map[string]manifest.Service{
			"agent": {Runtime: "container", Route: &manifest.Route{Port: 3008}},
			"db":    {Runtime: "container"},
		},
	}
	file := compose.File{
		Services: map[string]compose.Service{
			"agent": {Ports: []string{"3008:3008"}},
			"db":    {Ports: []string{"5432:5432"}},
		},
	}

	err := NewCaddyBackend(stack.Ingress).Contribute(stack, &file)
	if err != nil {
		t.Fatalf("Contribute() error = %v", err)
	}

	if _, ok := file.Networks["demo_edge"]; !ok {
		t.Fatal(`file.Networks["demo_edge"] missing`)
	}

	edge, ok := file.Services["edge"]
	if !ok {
		t.Fatal(`file.Services["edge"] missing`)
	}
	if want := []string{"443:443", "80:80"}; !reflect.DeepEqual(edge.Ports, want) {
		t.Fatalf("edge.Ports = %#v, want %#v", edge.Ports, want)
	}
	if !contains(edge.Volumes, "/var/run/docker.sock:/var/run/docker.sock:ro") {
		t.Fatalf("edge.Volumes = %#v, want docker socket mount", edge.Volumes)
	}
	if !contains(edge.Networks, "demo_edge") {
		t.Fatalf("edge.Networks = %#v, want demo_edge", edge.Networks)
	}

	agent := file.Services["agent"]
	if len(agent.Ports) != 0 {
		t.Fatalf("agent.Ports = %#v, want empty", agent.Ports)
	}
	if !contains(agent.Networks, "demo_edge") {
		t.Fatalf("agent.Networks = %#v, want demo_edge", agent.Networks)
	}
	if got, want := agent.Labels["caddy"], "agent.agents.localhost"; got != want {
		t.Fatalf(`agent.Labels["caddy"] = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.reverse_proxy"], "{{upstreams 3008}}"; got != want {
		t.Fatalf(`agent.Labels["caddy.reverse_proxy"] = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.reverse_proxy.flush_interval"], "-1"; got != want {
		t.Fatalf(`agent.Labels["caddy.reverse_proxy.flush_interval"] = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.import"], "forward_auth_edge agent"; got != want {
		t.Fatalf(`agent.Labels["caddy.import"] = %q, want %q`, got, want)
	}

	db := file.Services["db"]
	if want := []string{"5432:5432"}; !reflect.DeepEqual(db.Ports, want) {
		t.Fatalf("db.Ports = %#v, want %#v", db.Ports, want)
	}
	if contains(db.Networks, "demo_edge") {
		t.Fatalf("db.Networks = %#v, want no demo_edge", db.Networks)
	}
	if db.Labels != nil && db.Labels["caddy"] != "" {
		t.Fatalf(`db.Labels["caddy"] = %q, want empty`, db.Labels["caddy"])
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
