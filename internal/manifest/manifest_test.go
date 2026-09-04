package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")

	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "notes",
		SecretsBackend: SecretsBackend{
			Type: "env-file",
			Path: ".env",
		},
		Secrets: map[string]Secret{
			"postgres-password": {Generated: true, Length: 32},
		},
		Services: map[string]Service{
			"postgres": {
				Runtime: RuntimeContainer,
				Image:   "postgres:16",
				Env:     map[string]string{"POSTGRES_PASSWORD": "${secret.postgres-password}"},
			},
			"web": {
				Runtime: RuntimeLocal,
				Command: []string{"go", "run", "./cmd/web"},
				Workdir: "source://app",
			},
		},
	}

	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Name != "notes" {
		t.Fatalf("Name = %q, want notes", loaded.Name)
	}
	if loaded.Services["postgres"].Runtime != RuntimeContainer {
		t.Fatalf("postgres runtime = %q", loaded.Services["postgres"].Runtime)
	}
	if got := loaded.EnvFilePath(root); got != filepath.Join(root, ".env") {
		t.Fatalf("EnvFilePath() = %q", got)
	}
}

func TestLoadFileReadyProbeKinds(t *testing.T) {
	tests := map[string]string{
		"http": `
      http:
        port: 8080
`,
		"tcp": `
      tcp:
        port: 5432
`,
		"cmd": `
      cmd: ["sh", "-c", "test -s state/ready"]
`,
		"file": `
      file: state/ready
`,
	}
	for name, ready := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "angee.yaml")
			data := `version: 1
kind: stack
name: ready
services:
  web:
    runtime: container
    image: example/web:latest
    ready:` + ready
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			stack, err := LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			probe := stack.Services["web"].Ready
			if probe == nil {
				t.Fatal("Ready = nil")
			}
			normalized := probe.Normalized()
			if normalized.Interval != "5s" || normalized.Timeout != "3s" || normalized.Retries == nil || *normalized.Retries != 12 || normalized.StartPeriod != "0s" {
				t.Fatalf("Normalized() timing = %+v, want 5s/3s/12/0s", normalized)
			}
			if normalized.HTTP != nil && normalized.HTTP.Path != "/" {
				t.Fatalf("Normalized().HTTP.Path = %q, want /", normalized.HTTP.Path)
			}
		})
	}
}

func TestReadyProbeValidation(t *testing.T) {
	tests := map[string]struct {
		probe *ReadyProbe
		want  string
	}{
		"missing kind": {
			probe: &ReadyProbe{},
			want:  "exactly one",
		},
		"multiple kinds": {
			probe: &ReadyProbe{HTTP: &ReadyHTTP{Port: 8080}, TCP: &ReadyTCP{Port: 8080}},
			want:  "exactly one",
		},
		"http port below range": {
			probe: &ReadyProbe{HTTP: &ReadyHTTP{Port: 0}},
			want:  "ready.http.port",
		},
		"tcp port above range": {
			probe: &ReadyProbe{TCP: &ReadyTCP{Port: 65536}},
			want:  "ready.tcp.port",
		},
		"empty command": {
			probe: &ReadyProbe{Cmd: []string{}},
			want:  "ready.cmd",
		},
		"empty file": {
			probe: &ReadyProbe{File: "   "},
			want:  "ready.file",
		},
		"invalid interval": {
			probe: &ReadyProbe{TCP: &ReadyTCP{Port: 1}, Interval: "often"},
			want:  "ready.interval",
		},
		"invalid timeout": {
			probe: &ReadyProbe{TCP: &ReadyTCP{Port: 1}, Timeout: "eventually"},
			want:  "ready.timeout",
		},
		"invalid start period": {
			probe: &ReadyProbe{TCP: &ReadyTCP{Port: 1}, StartPeriod: "later"},
			want:  "ready.start_period",
		},
		"invalid retries": {
			probe: &ReadyProbe{TCP: &ReadyTCP{Port: 1}, Retries: readyRetries(0)},
			want:  "ready.retries",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			stack := &Stack{
				Version: VersionCurrent,
				Kind:    KindStack,
				Name:    "invalid-ready",
				Services: map[string]Service{
					"web": {Runtime: RuntimeContainer, Image: "example/web:latest", Ready: tc.probe},
				},
			}
			err := stack.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %q, want service name and %q", err, tc.want)
			}
		})
	}
}

func TestLoadFileReadyProbeRejectsZeroRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "angee.yaml")
	data := `version: 1
kind: stack
name: ready
services:
  web:
    runtime: container
    image: example/web:latest
    ready:
      tcp:
        port: 8080
      retries: 0
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), `service "web": ready.retries`) {
		t.Fatalf("LoadFile() error = %v, want named retries validation error", err)
	}
}

func TestLoadFileReadyProbeRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "angee.yaml")
	data := `version: 1
kind: stack
name: ready
services:
  web:
    runtime: container
    image: example/web:latest
    ready:
      tcp:
        port: 8080
      unknown: true
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("LoadFile() error = %v, want strict unknown-field error", err)
	}
}

func readyRetries(value int) *int {
	return &value
}

func TestIngressRoutingRoundTrip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")

	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "routed",
		Ingress: Ingress{Type: "caddy", Routing: "path", TLS: "off", Domain: "localhost"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008, Path: "/agent"},
			},
		},
	}

	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Ingress.Routing != "path" {
		t.Fatalf("Ingress.Routing = %q, want path", loaded.Ingress.Routing)
	}
	if loaded.Ingress.TLS != "off" {
		t.Fatalf("Ingress.TLS = %q, want off", loaded.Ingress.TLS)
	}
	if got := loaded.Services["agent"].Route.Path; got != "/agent" {
		t.Fatalf("Route.Path = %q, want /agent", got)
	}
}

// TestIngressByteStableWithoutRoutingFields guards the read-time-default
// decision: a caddy stack that omits routing/tls must marshal without emitting
// those keys, so existing manifests re-save unchanged.
func TestIngressByteStableWithoutRoutingFields(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "host-routed",
		Ingress: Ingress{Type: "caddy", Domain: "agents.localhost"},
		Services: map[string]Service{
			"agent": {
				Runtime: RuntimeContainer,
				Image:   "example/agent:latest",
				Route:   &Route{Port: 3008},
			},
		},
	}

	data, err := Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes.Contains(data, []byte("routing:")) {
		t.Fatalf("Marshal() emitted routing key:\n%s", data)
	}
	if bytes.Contains(data, []byte("tls:")) {
		t.Fatalf("Marshal() emitted tls key:\n%s", data)
	}
	if bytes.Contains(data, []byte("path:")) {
		t.Fatalf("Marshal() emitted route.path key:\n%s", data)
	}
}

func TestManifestRejectsInvalidLocalService(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "bad",
		Services: map[string]Service{
			"web": {Runtime: RuntimeLocal, Image: "example/web:latest"},
		},
	}
	if err := stack.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want error")
	}
}

// TestLoadFileToleratesLegacyLifecycleField guards the
// backwards-compat path for manifests written before commit f48784c
// (workspace lifecycle removal). Older files persist
// `workspaces[*].resolved.lifecycle`, which the strict YAML loader
// would otherwise reject. The field must load successfully and be
// silently dropped on the next save.
func TestLoadFileToleratesLegacyLifecycleField(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")
	legacy := `version: 1
kind: stack
name: legacy
workspaces:
  feature-a:
    template: workspaces/dev-pr
    resolved:
      chain_root: ".angee"
      lifecycle: auto
      allocations:
        custom: 10002
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	stack, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile rejected legacy lifecycle field: %v", err)
	}
	resolved := stack.Workspaces["feature-a"].Resolved
	if resolved.ChainRoot != ".angee" {
		t.Fatalf("ChainRoot = %q, want .angee", resolved.ChainRoot)
	}
	// LegacyLifecycle is intentionally not part of the persisted form;
	// saving must drop it from the file.
	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	roundtripped, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(roundtripped), "lifecycle") {
		t.Fatalf("saved manifest still carries lifecycle field:\n%s", roundtripped)
	}
}

func TestValidateDoesNotMutate(t *testing.T) {
	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "pure",
		SecretsBackend: SecretsBackend{
			Type: "env-file",
		},
		Services: map[string]Service{
			"web": {
				Runtime: RuntimeContainer,
				Image:   "nginx:latest",
				Ready:   &ReadyProbe{TCP: &ReadyTCP{Port: 8080}},
			},
		},
	}
	before, err := yaml.Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal(before) error = %v", err)
	}
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	after, err := yaml.Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal(after) error = %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("Validate() mutated stack\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestWorkspaceDefaultsRoundTrip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "angee.yaml")

	stack := &Stack{
		Version: VersionCurrent,
		Kind:    KindStack,
		Name:    "host",
		WorkspaceDefaults: map[string]WorkspaceDefaults{
			"workspaces/src": {Inputs: map[string]string{"work_state_source": "work-angee"}},
		},
	}
	if err := SaveFile(path, stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "workspace_defaults:\n    workspaces/src:\n        inputs:\n            work_state_source: work-angee\n") {
		t.Fatalf("workspace_defaults not serialized as expected:\n%s", data)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if got := loaded.WorkspaceDefaults["workspaces/src"].Inputs["work_state_source"]; got != "work-angee" {
		t.Fatalf("WorkspaceDefaults[workspaces/src].Inputs[work_state_source] = %q, want work-angee", got)
	}

	// A manifest without the block loads with an initialized, empty map and
	// never re-emits the key.
	bare := &Stack{Version: VersionCurrent, Kind: KindStack, Name: "bare"}
	if err := SaveFile(path, bare); err != nil {
		t.Fatalf("SaveFile(bare) error = %v", err)
	}
	loaded, err = LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(bare) error = %v", err)
	}
	if loaded.WorkspaceDefaults == nil || len(loaded.WorkspaceDefaults) != 0 {
		t.Fatalf("WorkspaceDefaults = %#v, want initialized empty map", loaded.WorkspaceDefaults)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "workspace_defaults") {
		t.Fatalf("bare manifest must not emit workspace_defaults:\n%s", data)
	}
}

func TestReadyProbeNormalizedHTTPPathGetsLeadingSlash(t *testing.T) {
	probe := ReadyProbe{HTTP: &ReadyHTTP{Port: 8080, Path: "healthz"}}
	if got := probe.Normalized().HTTP.Path; got != "/healthz" {
		t.Fatalf("normalized path = %q, want /healthz", got)
	}
	if probe.HTTP.Path != "healthz" {
		t.Fatal("Normalized must not mutate the probe")
	}
}
