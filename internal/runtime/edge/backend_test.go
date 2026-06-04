package edge

import (
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
)

func TestFromManifestNoneBackendsNoop(t *testing.T) {
	tests := []struct {
		name string
		cfg  manifest.Ingress
	}{
		{name: "empty", cfg: manifest.Ingress{Type: ""}},
		{name: "none", cfg: manifest.Ingress{Type: "none"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, err := FromManifest(tt.cfg)
			if err != nil {
				t.Fatalf("FromManifest() error = %v", err)
			}
			if backend == nil {
				t.Fatal("FromManifest() returned nil Backend")
			}

			before := testComposeFile()
			compiled := testComposeFile()
			if err := backend.Contribute(&manifest.Stack{Name: "test"}, &compiled); err != nil {
				t.Fatalf("Contribute() error = %v", err)
			}
			if !reflect.DeepEqual(compiled, before) {
				t.Fatalf("Contribute() mutated compose file:\n before: %#v\n  after: %#v", before, compiled)
			}
		})
	}
}

func TestFromManifestUnsupportedBackend(t *testing.T) {
	backend, err := FromManifest(manifest.Ingress{Type: "bogus"})
	if err == nil {
		t.Fatal("FromManifest() error = nil, want error")
	}
	if backend != nil {
		t.Fatalf("FromManifest() Backend = %#v, want nil", backend)
	}
}

func testComposeFile() compose.File {
	return compose.File{
		Name: "test",
		Services: map[string]compose.Service{
			"api": {
				Image: "example/api:latest",
				Ports: []string{"8080:8080"},
				Labels: map[string]string{
					"angee.test": "true",
				},
				Networks: []string{"default"},
			},
			"worker": {
				Image:   "example/worker:latest",
				Command: []string{"run", "worker"},
			},
		},
		Networks: map[string]compose.Network{
			"default": {},
		},
	}
}
