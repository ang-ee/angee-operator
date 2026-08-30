package service

import (
	"context"
	"io"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

// recordingBackend captures Up targets so tests can assert service selection.
type recordingBackend struct {
	stubStatusBackend
	targets []runtime.Target
}

func (b *recordingBackend) Up(_ context.Context, t runtime.Target) error {
	b.targets = append(b.targets, t)
	return nil
}

func (b *recordingBackend) UpForeground(_ context.Context, t runtime.Target, _ io.Writer, _ io.Writer) error {
	b.targets = append(b.targets, t)
	return nil
}

// A bare `angee up` must start the WHOLE compiled compose: the caddy edge is
// contributed at compile time and is absent from the manifest, so a
// manifest-derived selection silently skipped it (fresh stacks served no
// ingress until `angee dev` ran).
func TestStackUpWithoutSelectionStartsWholeCompose(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "demo",
		Services: map[string]manifest.Service{
			"web": {Runtime: manifest.RuntimeContainer, Image: "nginx"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	backend := &recordingBackend{}
	p, err := NewWithBackends(root, backend, stubStatusBackend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	if err := p.StackUp(context.Background(), nil, false); err != nil {
		t.Fatalf("StackUp: %v", err)
	}
	if len(backend.targets) != 1 || backend.targets[0].Services != nil {
		t.Fatalf("targets = %+v, want one unfiltered Up", backend.targets)
	}
	// An explicit selection still filters.
	if err := p.StackUp(context.Background(), []string{"web"}, false); err != nil {
		t.Fatalf("StackUp(web): %v", err)
	}
	if got := backend.targets[1].Services; len(got) != 1 || got[0] != "web" {
		t.Fatalf("explicit selection = %v, want [web]", got)
	}
}
