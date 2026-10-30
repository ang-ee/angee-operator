package substitute

import (
	"reflect"
	"testing"
)

func TestSecretRefs(t *testing.T) {
	got := SecretRefs(
		"CLAUDE_CODE_OAUTH_TOKEN=${secret.agent-x-inference}",
		"${secret.agent-x-inference}",           // duplicate, deduped
		"${secret.shared | required('set me')}", // pipe filter ignored
		"plain ${inputs.branch} ${ports.web}",   // non-secret namespaces ignored
		"${secret.}",                            // empty name ignored
	)
	want := []string{"agent-x-inference", "shared"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretRefs() = %v, want %v", got, want)
	}
}

func TestResolveSubstitutionsAndFilters(t *testing.T) {
	ctx := Context{
		Secrets: map[string]string{"token": "secret"},
		Ports:   map[string]int{"web": 8100},
		Inputs:  map[string]string{"branch": "Feat/Issue 123", "email": "dev@example.com"},
		Name:    "workspace-one",
		Operator: Operator{
			URL: "http://127.0.0.1:9000",
		},
	}

	got, err := Resolve("${inputs.branch | slug | truncate(12)}:${ports.web}:${inputs.email | local_part}:${operator.url}", ctx)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "feat-issue-1:8100:dev:http://127.0.0.1:9000" {
		t.Fatalf("Resolve() = %q", got)
	}
}

func TestResolveSecretEnvPlaceholder(t *testing.T) {
	ctx := Context{
		Secrets:       map[string]string{"postgres-password": "secret"},
		SecretEnvVars: map[string]string{"postgres-password": "ANGEE_SECRET_POSTGRES_PASSWORD"},
	}
	got, err := Resolve("password=${secret.postgres-password}", ctx)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "password=${ANGEE_SECRET_POSTGRES_PASSWORD}" {
		t.Fatalf("Resolve() = %q", got)
	}
}

func TestResolveCurrentWorkspacePath(t *testing.T) {
	got, err := Resolve("${workspace.path}/.angee/data", Context{
		WorkspacePath: "/tmp/workspaces/feature-a",
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := "/tmp/workspaces/feature-a/.angee/data"
	if got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolveStackRootInEnv(t *testing.T) {
	// A service passing its own root as an argument (env value form).
	got, err := Resolve("--root=${stack.root}", Context{Root: "/srv/stacks/demo"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := "--root=/srv/stacks/demo"; got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolveStackRootInMount(t *testing.T) {
	// The identical-path mount the operator service declares: the container
	// side is the absolute host root, which the token supplies.
	got, err := Resolve("bind://.:${stack.root}", Context{Root: "/srv/stacks/demo"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := "bind://.:/srv/stacks/demo"; got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolveStackRootUnresolved(t *testing.T) {
	if _, err := Resolve("${stack.root}", Context{}); err == nil {
		t.Fatal("Resolve() error = nil, want error for unresolved stack root")
	}
	if _, err := Resolve("${stack.branch}", Context{Root: "/srv/demo"}); err == nil {
		t.Fatal("Resolve() error = nil, want error for unknown stack field")
	}
}

func TestRequiredFilterRejectsEmpty(t *testing.T) {
	_, err := Resolve("${name | required('name required')}", Context{})
	if err == nil {
		t.Fatal("Resolve() error = nil, want error")
	}
}

func TestSecretEnvName(t *testing.T) {
	if got := SecretEnvName("postgres-password"); got != "ANGEE_SECRET_POSTGRES_PASSWORD" {
		t.Fatalf("SecretEnvName() = %q", got)
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	got, err := Resolve("${inputs.value | truncate(2)}", Context{Inputs: map[string]string{"value": "åßcd"}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "åß" {
		t.Fatalf("Resolve() = %q", got)
	}
}

func TestDefaultedMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name, expression string
		inputs           map[string]string
		want             string
		failure          bool
	}{
		{"missing", "${inputs.value | default('run')}", nil, "run", false},
		{"missing-empty-default", "${inputs.value | default('')}", nil, "", false},
		{"present-empty", "${inputs.value | default('run')}", map[string]string{"value": ""}, "run", false},
		{"present-false", "${inputs.value | default('run')}", map[string]string{"value": "false"}, "false", false},
		{"present-zero", "${inputs.value | default('run')}", map[string]string{"value": "0"}, "0", false},
		{"present-value", "${inputs.value | default('run') | upper}", map[string]string{"value": "chosen"}, "CHOSEN", false},
		{"required-input", "${inputs.value}", nil, "", true},
		{"no-leading-default", "${inputs.value | lower | default('run')}", nil, "", true},
		{"missing-secret", "${secret.value | default('unsafe')}", nil, "", true},
		{"missing-port", "${ports.value | default('9000')}", nil, "", true},
		{"invalid-default", "${inputs.value | default()}", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.expression, Context{Inputs: tc.inputs})
			if (err != nil) != tc.failure {
				t.Fatalf("Resolve = %q, %v; failure=%v", got, err, tc.failure)
			}
			if !tc.failure && got != tc.want {
				t.Fatalf("Resolve = %q, want %q", got, tc.want)
			}
		})
	}
}
