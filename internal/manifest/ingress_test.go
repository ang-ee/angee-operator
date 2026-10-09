package manifest

import (
	"strings"
	"testing"
)

func TestDefaultsSetsIngressTypeNone(t *testing.T) {
	stack := &Stack{}

	stack.Defaults()

	if stack.Ingress.Type != "none" {
		t.Fatalf("Ingress.Type = %q, want none", stack.Ingress.Type)
	}
}

func TestValidateRejectsRouteOnLocalService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "routed-local",
		Services: map[string]Service{
			"web": {
				Runtime: RuntimeLocal,
				Command: []string{"npm", "run", "dev"},
				Route:   &Route{Port: 3000},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "route") {
		t.Fatalf("Validate() error = %q, want mention of route", err.Error())
	}
}

func TestValidateAllowsRouteOnContainerService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "routed-container",
		Services: map[string]Service{
			"web": {
				Runtime: RuntimeContainer,
				Image:   "example/web:latest",
				Route:   &Route{Port: 3000},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsCaddyRouteHostWithCaddyMeta(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad-host",
		Ingress: Ingress{Type: "caddy"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Host: "evil.com { respond 200 }"},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "route.host") {
		t.Fatalf("Validate() error = %q, want route.host rejection", err.Error())
	}
}

func TestValidateRejectsCaddyRoutedServiceNamedEdge(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "edge-name",
		Ingress: Ingress{Type: "caddy"},
		Services: map[string]Service{
			"edge": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate() error = %q, want reserved-name rejection", err.Error())
	}
}

func TestValidateAllowsCleanCaddyRoutedService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "clean-caddy",
		Ingress: Ingress{Type: "caddy"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Host: "agent.agents.localhost"},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAllowsCleanCaddyPathRoute(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "clean-path",
		Ingress: Ingress{Type: "caddy", Routing: "path", TLS: "off", Domain: "localhost"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Path: "/agent"},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateAllowsSingleRootPathRoute(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "single-root-path",
		Ingress: Ingress{Type: "caddy", Routing: "path", Domain: "dev.example.com"},
		Services: map[string]Service{
			"frontend": {
				Runtime: RuntimeContainer,
				Image:   "example/frontend:latest",
				Route:   &Route{Port: 5173, Path: "/", Auth: "none"},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsMultipleRootPathRoutes(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "multiple-root-paths",
		Ingress: Ingress{Type: "caddy", Routing: "path", Domain: "dev.example.com"},
		Services: map[string]Service{
			"frontend": {
				Runtime: RuntimeContainer,
				Image:   "example/frontend:latest",
				Route:   &Route{Port: 5173, Path: "/", Auth: "none"},
			},
			"other": {
				Runtime: RuntimeContainer,
				Image:   "example/other:latest",
				Route:   &Route{Port: 3000, Path: "///"},
			},
		},
	}

	err := stack.Validate()
	if err == nil || !strings.Contains(err.Error(), "multiple services route the ingress root") {
		t.Fatalf("Validate() error = %v, want multiple-root rejection", err)
	}
}

func TestValidateAcceptsIngressPortWithTLSOff(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "edge-port",
		Ingress: Ingress{Type: "caddy", Routing: "path", TLS: "off", Domain: "localhost", Port: 7003},
		Services: map[string]Service{
			"agent": {Runtime: RuntimeContainer, Image: "example/agent:latest", Route: &Route{Port: 3008}},
		},
	}
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsIngressPortWithoutTLSOff(t *testing.T) {
	// tls defaults to auto; a port there would be a silent no-op, so reject it.
	stack := &Stack{
		Version:  VersionCurrent,
		Kind:     KindStack,
		Name:     "edge-port-tls-auto",
		Ingress:  Ingress{Type: "caddy", Domain: "localhost", Port: 7003},
		Services: map[string]Service{"agent": {Runtime: RuntimeContainer, Image: "x:latest", Route: &Route{Port: 3008}}},
	}
	err := stack.Validate()
	if err == nil || !strings.Contains(err.Error(), "ingress.tls: off") {
		t.Fatalf("Validate() error = %v, want ingress.tls: off rejection", err)
	}
}

func TestValidateRejectsIngressPortWhenTypeNotCaddy(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "edge-port-no-edge",
		Ingress: Ingress{Type: "none", Port: 7003},
	}
	err := stack.Validate()
	if err == nil || !strings.Contains(err.Error(), "ingress.type: caddy") {
		t.Fatalf("Validate() error = %v, want ingress.type: caddy rejection", err)
	}
}

func TestValidateRejectsIngressPortOutOfRange(t *testing.T) {
	stack := &Stack{
		Version:  VersionCurrent,
		Kind:     KindStack,
		Name:     "edge-port-range",
		Ingress:  Ingress{Type: "caddy", Routing: "path", TLS: "off", Domain: "localhost", Port: 70000},
		Services: map[string]Service{"agent": {Runtime: RuntimeContainer, Image: "x:latest", Route: &Route{Port: 3008}}},
	}
	if err := stack.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want out-of-range port rejection")
	}
}

func TestValidateRejectsRouteHostAndPathTogether(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "host-and-path",
		Ingress: Ingress{Type: "caddy", Routing: "path", Domain: "localhost"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Host: "agent.localhost", Path: "/agent"},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Validate() error = %q, want mutually-exclusive rejection", err.Error())
	}
}

func TestValidatePathModeRequiresDomain(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "path-no-domain",
		Ingress: Ingress{Type: "caddy", Routing: "path"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "domain") {
		t.Fatalf("Validate() error = %q, want domain requirement", err.Error())
	}
}

func TestValidateRejectsRoutePathWithCaddyMeta(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad-path",
		Ingress: Ingress{Type: "caddy", Routing: "path", Domain: "localhost"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Path: "/agent { respond 200 }"},
			},
		},
	}

	err := stack.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "route.path") {
		t.Fatalf("Validate() error = %q, want route.path rejection", err.Error())
	}
}

func TestValidateRejectsUnknownRoutingMode(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad-routing",
		Ingress: Ingress{Type: "caddy", Routing: "subdomain", Domain: "localhost"},
	}

	if err := stack.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want error for unknown routing mode")
	}
}

func TestRoutingAndTLSModeDefaults(t *testing.T) {
	var ing Ingress
	if got := ing.RoutingMode(); got != "host" {
		t.Fatalf("RoutingMode() = %q, want host", got)
	}
	if got := ing.TLSMode(); got != "auto" {
		t.Fatalf("TLSMode() = %q, want auto", got)
	}
	ing = Ingress{Routing: "path", TLS: "off"}
	if got := ing.RoutingMode(); got != "path" {
		t.Fatalf("RoutingMode() = %q, want path", got)
	}
	if got := ing.TLSMode(); got != "off" {
		t.Fatalf("TLSMode() = %q, want off", got)
	}
}

func TestRoutePathPrefix(t *testing.T) {
	tests := []struct {
		path    string
		service string
		want    string
	}{
		{path: "", service: "agent", want: "/agent"},
		{path: "/agent", service: "agent", want: "/agent"},
		{path: "agent", service: "agent", want: "/agent"},
		{path: "/v1/agent/", service: "agent", want: "/v1/agent"},
		{path: "/", service: "frontend", want: ""},
		{path: "///", service: "frontend", want: ""},
	}
	for _, tt := range tests {
		r := &Route{Port: 3008, Path: tt.path}
		if got := r.PathPrefix(tt.service); got != tt.want {
			t.Fatalf("PathPrefix(%q, path=%q) = %q, want %q", tt.service, tt.path, got, tt.want)
		}
	}
}

func TestValidateAllowsBadRouteHostWhenIngressNone(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad-host-no-caddy",
		Ingress: Ingress{Type: "none"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Host: "evil.com { respond 200 }"},
			},
		},
	}

	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateIngressAliases(t *testing.T) {
	stackWith := func(ingress Ingress, operatorDomain string) *Stack {
		return &Stack{
			Version:  VersionCurrent,
			Kind:     KindStack,
			Name:     "aliases",
			Operator: Operator{Domain: operatorDomain},
			Ingress:  ingress,
			Services: map[string]Service{"web": {Runtime: RuntimeContainer, Image: "x:latest", Route: &Route{Port: 80}}},
		}
	}
	pathIngress := func(aliases ...string) Ingress {
		return Ingress{Type: "caddy", Routing: "path", Domain: "angee.phro.gg", Aliases: aliases}
	}
	for _, tc := range []struct {
		name    string
		stack   *Stack
		wantErr string
	}{
		{name: "path routing", stack: stackWith(pathIngress("ap.phro.gg", "painkiller.phro.gg"), "")},
		{name: "operator domain when ingress has none", stack: stackWith(Ingress{Type: "caddy", Routing: "path", Aliases: []string{"ap.phro.gg"}}, "angee.phro.gg")},
		{name: "operator domain is not the edge's", stack: stackWith(pathIngress("ops.phro.gg"), "ops.phro.gg")},
		{name: "host routing", stack: stackWith(Ingress{Type: "caddy", Routing: "host", Domain: "angee.phro.gg", Aliases: []string{"ap.phro.gg"}}, ""), wantErr: "ingress.routing: path"},
		{name: "no edge", stack: stackWith(Ingress{Type: "none", Aliases: []string{"ap.phro.gg"}}, ""), wantErr: "ingress.type: caddy"},
		{name: "not a host name", stack: stackWith(pathIngress("https://ap.phro.gg"), ""), wantErr: `"https://ap.phro.gg" is not a host name`},
		{name: "empty label", stack: stackWith(pathIngress("ap..phro.gg"), ""), wantErr: `"ap..phro.gg" is not a host name`},
		{name: "trailing dot", stack: stackWith(pathIngress("ap.phro.gg."), ""), wantErr: "is not a host name"},
		{name: "label starts with a hyphen", stack: stackWith(pathIngress("-ap.phro.gg"), ""), wantErr: "is not a host name"},
		{name: "label ends with a hyphen", stack: stackWith(pathIngress("ap-.phro.gg"), ""), wantErr: "is not a host name"},
		{name: "label too long", stack: stackWith(pathIngress(strings.Repeat("a", 64)+".phro.gg"), ""), wantErr: "is not a host name"},
		{name: "hyphen inside a label", stack: stackWith(pathIngress("pain-killer.phro.gg"), "")},
		{name: "the domain", stack: stackWith(pathIngress("Angee.Phro.gg"), ""), wantErr: "repeats the domain or another alias"},
		{name: "a repeat", stack: stackWith(pathIngress("ap.phro.gg", "AP.phro.gg"), ""), wantErr: "repeats the domain or another alias"},
		{name: "the operator domain it falls back to", stack: stackWith(Ingress{Type: "caddy", Routing: "path", Aliases: []string{"angee.phro.gg"}}, "angee.phro.gg"), wantErr: "repeats the domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.stack.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
