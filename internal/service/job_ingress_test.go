package service

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

// routedChainStack is a caddy-ingress stack where a chained restart of the deps
// job reaches only the routed frontend; api and zeta are routed services the
// run leaves alone, named to sort on either side of frontend so path routing's
// label numbering differs between the whole stack and the run's view.
func routedChainStack(t *testing.T, routing string) *manifest.Stack {
	t.Helper()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "edge-labels",
		Ingress: manifest.Ingress{Type: "caddy", Routing: routing, Domain: "app.localhost"},
		Jobs: map[string]manifest.Job{
			"deps": {Runtime: manifest.RuntimeLocal, Command: []string{"true"}},
		},
		Services: map[string]manifest.Service{
			"api":      {Runtime: manifest.RuntimeContainer, Image: "nginx:latest", Route: &manifest.Route{Port: 8000}},
			"frontend": {Runtime: manifest.RuntimeContainer, Image: "node:22", Route: &manifest.Route{Port: 5173}, DependsOn: []string{"deps"}},
			"zeta":     {Runtime: manifest.RuntimeContainer, Image: "nginx:latest", Route: &manifest.Route{Port: 7000}},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	return stack
}

// A chained restart recreates the routed services it reaches from the job
// run's compiled view, so that view must give them exactly the ingress wiring
// a full compile does (labels, edge network, no published ports), numbered over
// the whole stack, and still leave out the stack-wide edge service.
func TestJobRunViewKeepsRoutedServiceIngressWiring(t *testing.T) {
	for _, routing := range []string{"host", "path"} {
		t.Run(routing, func(t *testing.T) {
			stack := routedChainStack(t, routing)
			root := t.TempDir()
			if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
				t.Fatalf("SaveFile: %v", err)
			}
			backend := &recordingJobBackend{}
			p, err := NewWithBackends(root, backend, backend)
			if err != nil {
				t.Fatalf("NewWithBackends: %v", err)
			}
			view, err := p.stackPrepare(context.Background(), nil, false, "deps", true, true)
			if err != nil {
				t.Fatalf("stackPrepare(deps, chained) error = %v", err)
			}
			full, err := Compile(stack, root, nil)
			if err != nil {
				t.Fatalf("Compile() error = %v", err)
			}

			got, ok := view.Compose.Services["frontend"]
			if !ok {
				t.Fatalf("job run view has no frontend service: %#v", view.Compose.Services)
			}
			want := full.Compose.Services["frontend"]
			if want.Labels["caddy"] == "" {
				t.Fatalf("full compile frontend labels = %#v, want caddy labels", want.Labels)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("job run view frontend =\n%#v\nwant the full compile's\n%#v", got, want)
			}
			for _, absent := range []string{"edge", "api", "zeta"} {
				if _, ok := view.Compose.Services[absent]; ok {
					t.Fatalf("job run view has service %q, want only the run's nodes", absent)
				}
			}
			for _, network := range want.Networks {
				if _, ok := view.Compose.Networks[network]; !ok {
					t.Fatalf("job run view networks = %#v, want %q declared", view.Compose.Networks, network)
				}
			}
		})
	}
}

// applyCapturingBackend records the in-memory configuration each service apply
// is given and reports every service running and healthy.
type applyCapturingBackend struct {
	recordingJobBackend
	mu      sync.Mutex
	applied map[string][]byte
}

func (b *applyCapturingBackend) Apply(_ context.Context, target runtime.Target) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.applied == nil {
		b.applied = map[string][]byte{}
	}
	b.applied[target.Services[0]] = append([]byte(nil), target.Configuration...)
	return nil
}

func (b *applyCapturingBackend) configuration(service string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applied[service]
}

// The chained restart itself, end to end: the configuration the compose
// backend recreates the routed service from carries its caddy labels.
func TestChainedRestartRecreatesRoutedServiceWithCaddyLabels(t *testing.T) {
	stack := routedChainStack(t, "host")
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	backend := &applyCapturingBackend{}
	backend.statuses = []runtime.ServiceStatus{{Name: "frontend", State: "running"}}
	p, err := NewWithBackends(root, backend, backend)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	receipt, err := p.JobRunStart(context.Background(), "deps", nil, true)
	if err != nil {
		t.Fatalf("JobRunStart(deps, chained) error = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for receipt.Status == api.JobRunPending || receipt.Status == api.JobRunRunning {
		if time.Now().After(deadline) {
			t.Fatalf("operation did not finish: %#v", receipt)
		}
		time.Sleep(5 * time.Millisecond)
		if receipt, err = p.JobRunGet(context.Background(), receipt.ID); err != nil {
			t.Fatalf("JobRunGet: %v", err)
		}
	}
	if receipt.Status != api.JobRunSucceeded {
		t.Fatalf("operation = %#v, want success", receipt)
	}
	configuration := string(backend.configuration("frontend"))
	if configuration == "" {
		t.Fatalf("frontend was not applied from an in-memory configuration")
	}
	for _, label := range []string{"caddy: frontend.app.localhost", "caddy.reverse_proxy:", "caddy.forward_auth:"} {
		if !strings.Contains(configuration, label) {
			t.Fatalf("configuration frontend was recreated from lacks %q:\n%s", label, configuration)
		}
	}
}
