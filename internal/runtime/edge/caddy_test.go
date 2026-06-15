package edge

import (
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
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
	if got, want := agent.Labels["caddy.forward_auth"], "operator:9000"; got != want {
		t.Fatalf(`agent.Labels["caddy.forward_auth"] = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.forward_auth.uri"], "/edge/verify?service=agent"; got != want {
		t.Fatalf(`agent.Labels["caddy.forward_auth.uri"] = %q, want %q`, got, want)
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

func TestCaddyBackend_ContributePathMode(t *testing.T) {
	stack := &manifest.Stack{
		Name: "demo",
		Ingress: manifest.Ingress{
			Type:    "caddy",
			Routing: "path",
			TLS:     "off",
			Domain:  "localhost",
		},
		Services: map[string]manifest.Service{
			"agent": {Runtime: "container", Route: &manifest.Route{Port: 3008}},
			"chat":  {Runtime: "container", Route: &manifest.Route{Port: 3009}},
			"db":    {Runtime: "container"},
		},
	}
	file := compose.File{
		Services: map[string]compose.Service{
			"agent": {Ports: []string{"3008:3008"}},
			"chat":  {Ports: []string{"3009:3009"}},
			"db":    {Ports: []string{"5432:5432"}},
		},
	}

	err := NewCaddyBackend(stack.Ingress).Contribute(stack, &file)
	if err != nil {
		t.Fatalf("Contribute() error = %v", err)
	}

	// tls: off -> edge publishes only the HTTP port.
	edge := file.Services["edge"]
	if want := []string{"80:80"}; !reflect.DeepEqual(edge.Ports, want) {
		t.Fatalf("edge.Ports = %#v, want %#v", edge.Ports, want)
	}

	agent := file.Services["agent"]
	chat := file.Services["chat"]

	// Both routed services share one HTTP site address.
	if got, want := agent.Labels["caddy"], "http://localhost"; got != want {
		t.Fatalf(`agent.Labels["caddy"] = %q, want %q`, got, want)
	}
	if got, want := chat.Labels["caddy"], "http://localhost"; got != want {
		t.Fatalf(`chat.Labels["caddy"] = %q, want %q`, got, want)
	}

	// Deterministic, distinct, non-colliding handle_path prefixes (sorted:
	// agent=0, chat=1).
	if got, want := agent.Labels["caddy.0_handle_path"], "/agent/*"; got != want {
		t.Fatalf(`agent.Labels["caddy.0_handle_path"] = %q, want %q`, got, want)
	}
	if got, want := chat.Labels["caddy.1_handle_path"], "/chat/*"; got != want {
		t.Fatalf(`chat.Labels["caddy.1_handle_path"] = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.0_handle_path.reverse_proxy"], "{{upstreams 3008}}"; got != want {
		t.Fatalf(`agent reverse_proxy label = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.0_handle_path.reverse_proxy.flush_interval"], "-1"; got != want {
		t.Fatalf(`agent flush_interval label = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.0_handle_path.forward_auth"], "operator:9000"; got != want {
		t.Fatalf(`agent forward_auth label = %q, want %q`, got, want)
	}
	if got, want := agent.Labels["caddy.0_handle_path.forward_auth.uri"], "/edge/verify?service=agent"; got != want {
		t.Fatalf(`agent forward_auth.uri label = %q, want %q`, got, want)
	}
	if got, want := chat.Labels["caddy.1_handle_path.forward_auth.uri"], "/edge/verify?service=chat"; got != want {
		t.Fatalf(`chat forward_auth.uri label = %q, want %q`, got, want)
	}

	// Path mode must not emit host-mode top-level directives.
	if _, ok := agent.Labels["caddy.reverse_proxy"]; ok {
		t.Fatalf("agent emitted host-mode caddy.reverse_proxy label: %#v", agent.Labels)
	}

	// Routed services lose their published ports; non-routed are untouched.
	if len(agent.Ports) != 0 {
		t.Fatalf("agent.Ports = %#v, want empty", agent.Ports)
	}
	db := file.Services["db"]
	if want := []string{"5432:5432"}; !reflect.DeepEqual(db.Ports, want) {
		t.Fatalf("db.Ports = %#v, want %#v", db.Ports, want)
	}
	if db.Labels["caddy"] != "" {
		t.Fatalf(`db.Labels["caddy"] = %q, want empty`, db.Labels["caddy"])
	}
}

func TestCaddyBackend_ContributeHostModeTLSOff(t *testing.T) {
	// tls is orthogonal to routing: host mode + tls:off serves the per-service
	// subdomain over plain HTTP (ws://) on port 80.
	stack := &manifest.Stack{
		Name:    "demo",
		Ingress: manifest.Ingress{Type: "caddy", TLS: "off", Domain: "agents.localhost"},
		Services: map[string]manifest.Service{
			"agent": {Runtime: "container", Route: &manifest.Route{Port: 3008}},
		},
	}
	file := compose.File{
		Services: map[string]compose.Service{
			"agent": {Ports: []string{"3008:3008"}},
		},
	}

	if err := NewCaddyBackend(stack.Ingress).Contribute(stack, &file); err != nil {
		t.Fatalf("Contribute() error = %v", err)
	}

	if want := []string{"80:80"}; !reflect.DeepEqual(file.Services["edge"].Ports, want) {
		t.Fatalf("edge.Ports = %#v, want %#v", file.Services["edge"].Ports, want)
	}
	if got, want := file.Services["agent"].Labels["caddy"], "http://agent.agents.localhost"; got != want {
		t.Fatalf(`agent.Labels["caddy"] = %q, want %q`, got, want)
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
