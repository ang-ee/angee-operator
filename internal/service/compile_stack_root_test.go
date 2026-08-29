package service

import (
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// The operator service declares an identical-path bind mount: the container
// side must equal the absolute host stack root, which a template cannot know
// at render time. `${stack.root}` supplies it. The source stays the relative
// `.`, which docker resolves against the compose file's directory (the stack
// root) — so at runtime the mount is <root>:<root>, letting an in-container
// operator's generated host-pathed compose files agree with the host daemon.
func TestCompileStackRootMountAndEnv(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Name: "demo",
		Services: map[string]manifest.Service{
			"operator": {
				Runtime: manifest.RuntimeContainer,
				Image:   "angee/operator:latest",
				Mounts:  manifest.StringList{"bind://.:${stack.root}"},
				Env:     map[string]string{"ANGEE_ROOT": "${stack.root}"},
				Command: []string{"angee", "--root", "${stack.root}", "serve"},
			},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	compiled, err := Compile(stack, root, nil)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	operator := compiled.Compose.Services["operator"]
	// The relative `.` source is preserved; the container target is the
	// absolute host root. Docker resolves `.` to the compose dir (= root),
	// making this the identical-path mount root:root at runtime.
	if want := []string{".:" + root}; len(operator.Volumes) != 1 || operator.Volumes[0] != want[0] {
		t.Fatalf("operator.Volumes = %#v, want %#v", operator.Volumes, want)
	}
	if got := operator.Environment["ANGEE_ROOT"]; got != root {
		t.Fatalf("operator.Environment[ANGEE_ROOT] = %q, want %q", got, root)
	}
	if want := []string{"angee", "--root", root, "serve"}; len(operator.Command) != len(want) {
		t.Fatalf("operator.Command = %#v, want %#v", operator.Command, want)
	} else {
		for i := range want {
			if operator.Command[i] != want[i] {
				t.Fatalf("operator.Command = %#v, want %#v", operator.Command, want)
			}
		}
	}
}

// A stack with zero runtime:local services still compiles a well-formed,
// empty process-compose document (no processes) with no side effects, while
// the container service lands in the compose file. The runtime backends and
// artifact writers gate on len(local)>0 / len(Processes)>0, so this empty
// document is simply never written or launched.
func TestCompileDockerOnlyStackHasEmptyProcessCompose(t *testing.T) {
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
	if len(compiled.ProcessCompose.Processes) != 0 {
		t.Fatalf("ProcessCompose.Processes = %#v, want empty", compiled.ProcessCompose.Processes)
	}
	if compiled.ProcessCompose.Version == "" {
		t.Fatal("ProcessCompose.Version is empty, want a well-formed document")
	}
	if _, ok := compiled.Compose.Services["web"]; !ok {
		t.Fatal(`compiled.Compose.Services["web"] missing`)
	}
}
