package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestWorkspaceCreatePreflightFlagsMissingRequired(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
    branch:
      required: true
      default: main
    tier:
      type: int
`)
	p, err := New(root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"tier": "10"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.OK {
		t.Fatalf("OK = true, want false (topic is required)")
	}
	if len(resp.MissingRequired) != 1 || resp.MissingRequired[0] != "topic" {
		t.Fatalf("MissingRequired = %v, want [topic]", resp.MissingRequired)
	}
	if got := resp.EffectiveInputs["branch"]; got != "main" {
		t.Fatalf("EffectiveInputs[branch] = %q, want main (default)", got)
	}
	if got := resp.EffectiveInputs["tier"]; got != "10" {
		t.Fatalf("EffectiveInputs[tier] = %q, want 10 (provided)", got)
	}
}

func TestWorkspaceCreatePreflightFlagsTypeMismatch(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    count:
      type: int
    enabled:
      type: bool
`)
	p, _ := New(root)
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"count": "abc", "enabled": "maybe"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.OK {
		t.Fatalf("OK = true, want false")
	}
	if len(resp.InvalidInputs) != 2 {
		t.Fatalf("InvalidInputs = %v, want 2 entries", resp.InvalidInputs)
	}
	// Sorted alphabetically: count, enabled.
	if resp.InvalidInputs[0].Field != "count" || resp.InvalidInputs[1].Field != "enabled" {
		t.Fatalf("InvalidInputs ordering = %+v", resp.InvalidInputs)
	}
}

func TestWorkspaceCreatePreflightOKWhenSatisfied(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
`)
	p, _ := New(root)
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "workspaces/dev-pr",
		Inputs:   map[string]string{"topic": "feature"},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("OK = false, want true (got missing=%v, invalid=%v)", resp.MissingRequired, resp.InvalidInputs)
	}
	if resp.ResolvedTemplate == "" {
		t.Fatalf("ResolvedTemplate empty, want resolved ref")
	}
}

func writePreflightTemplate(t *testing.T, root, copierYAML string) {
	t.Helper()
	templateRoot := filepath.Join(root, ".templates", "workspaces", "dev-pr")
	if err := os.MkdirAll(filepath.Join(templateRoot, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	full := `_subdirectory: template
_templates_suffix: .jinja
` + copierYAML
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(full), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
}

func TestWorkspaceCreatePreflightAppliesStackWorkspaceDefaults(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
    work_state_source:
      type: str
      default: ""
`)
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "host",
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{
			"dev-pr": {Inputs: map[string]string{"topic": "from-stack", "work_state_source": "work-angee"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	p, _ := New(root)

	// The stack default satisfies the required input on its own.
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "workspaces/dev-pr"})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if !resp.OK {
		t.Fatalf("OK = false, want true (missing=%v, invalid=%v)", resp.MissingRequired, resp.InvalidInputs)
	}
	if resp.EffectiveInputs["topic"] != "from-stack" || resp.EffectiveInputs["work_state_source"] != "work-angee" {
		t.Fatalf("EffectiveInputs = %v, want the stack defaults applied", resp.EffectiveInputs)
	}
	if resp.StackDefaults["work_state_source"] != "work-angee" {
		t.Fatalf("StackDefaults = %v, want the stack's inputs reported", resp.StackDefaults)
	}

	// An explicit input, even an empty one, wins over the stack default.
	resp, err = p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
		Template: "dev-pr",
		Inputs:   map[string]string{"topic": "mine", "work_state_source": ""},
	})
	if err != nil {
		t.Fatalf("WorkspaceCreatePreflight() error = %v", err)
	}
	if resp.EffectiveInputs["topic"] != "mine" || resp.EffectiveInputs["work_state_source"] != "" {
		t.Fatalf("EffectiveInputs = %v, want explicit inputs to win", resp.EffectiveInputs)
	}
}

func TestWorkspaceCreatePreflightAppliesQuestionDefaults(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      required: true
      default: metadata-topic
    branch:
      required: true
      default: main
topic:
  required: true
  default: question-topic
count:
  type: int
  required: true
  default: 4
enabled:
  type: bool
  required: true
  default: false
`)
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "dev-pr"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"topic": "question-topic", "branch": "main", "count": "4", "enabled": "false"}
	if !resp.OK || !reflect.DeepEqual(resp.EffectiveInputs, want) {
		t.Fatalf("preflight = %#v, want OK with effective inputs %#v", resp, want)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "host",
		WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{
			"dev-pr": {Inputs: map[string]string{"topic": "stack-topic"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inputs map[string]string
		want   string
		ok     bool
	}{
		{name: "stack", want: "stack-topic", ok: true},
		{name: "request", inputs: map[string]string{"topic": "request-topic"}, want: "request-topic", ok: true},
		{name: "explicit empty", inputs: map[string]string{"topic": ""}, want: "", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "dev-pr", Inputs: tc.inputs})
			if err != nil {
				t.Fatal(err)
			}
			if resp.OK != tc.ok || resp.EffectiveInputs["topic"] != tc.want {
				t.Fatalf("preflight = %#v, want OK=%t and topic=%q", resp, tc.ok, tc.want)
			}
		})
	}
}

func TestWorkspaceCreatePreflightQuestionWithoutDefaultOverridesMetadata(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    topic:
      default: metadata-topic
topic:
  required: true
`)
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "dev-pr"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || !reflect.DeepEqual(resp.MissingRequired, []string{"topic"}) {
		t.Fatalf("preflight = %#v, want missing topic", resp)
	}
}

func TestWorkspaceCreatePreflightSkipsAbsentGeneratedRequired(t *testing.T) {
	root := t.TempDir()
	writePreflightTemplate(t, root, `_angee:
  kind: workspace
  name: dev-pr
  inputs:
    metadata_token:
      generated: true
      required: true
token:
  generated: true
  required: true
`)
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		inputs  map[string]string
		missing []string
	}{
		{name: "generated during render"},
		{name: "explicit empty", inputs: map[string]string{"token": ""}, missing: []string{"token"}},
		{name: "provided", inputs: map[string]string{"token": "provided"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{Template: "dev-pr", Inputs: tc.inputs})
			if err != nil {
				t.Fatal(err)
			}
			if resp.OK != (len(tc.missing) == 0) || len(resp.MissingRequired) != len(tc.missing) {
				t.Fatalf("preflight = %#v, want missing %v", resp, tc.missing)
			}
			for i, name := range tc.missing {
				if resp.MissingRequired[i] != name {
					t.Fatalf("MissingRequired = %v, want %v", resp.MissingRequired, tc.missing)
				}
			}
			if _, ok := resp.EffectiveInputs["metadata_token"]; ok {
				t.Fatalf("preflight generated an input: %#v", resp.EffectiveInputs)
			}
		})
	}
}

func TestWorkspaceCreatePreflightValidatesMultiselectItems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		typ    string
		value  string
		reason string
	}{
		{name: "integers", typ: "int", value: `["1","2"]`},
		{name: "booleans", typ: "bool", value: `["true","false"]`},
		{name: "strings without type", value: `["a","b"]`},
		{name: "empty list", typ: "int", value: `[]`},
		{name: "invalid integer", typ: "int", value: `["1","bad"]`, reason: `not an integer: "bad"`},
		{name: "invalid boolean", typ: "bool", value: `["true","maybe"]`, reason: `not a boolean: "maybe"`},
		{name: "scalar", typ: "int", value: `"1"`, reason: "must be a JSON array of strings"},
		{name: "numeric list", typ: "int", value: `[1,2]`, reason: "must be a JSON array of strings"},
		{name: "null", typ: "bool", value: `null`, reason: "must be a JSON array of strings"},
		{name: "null selection", value: `[null]`, reason: "must be a JSON array of strings"},
		{name: "malformed without type", value: `["a"`, reason: "must be a JSON array of strings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			question := "items:\n  multiselect: true\n"
			if tc.typ != "" {
				question += "  type: " + tc.typ + "\n"
			}
			writePreflightTemplate(t, root, "_angee:\n  kind: workspace\n  name: dev-pr\n"+question)
			p, err := New(root)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.WorkspaceCreatePreflight(context.Background(), api.WorkspaceCreateRequest{
				Template: "dev-pr",
				Inputs:   map[string]string{"items": tc.value},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.OK != (tc.reason == "") {
				t.Fatalf("preflight = %#v, want reason %q", resp, tc.reason)
			}
			if tc.reason != "" {
				want := []api.PreflightFailure{{Field: "items", Reason: tc.reason}}
				if !reflect.DeepEqual(resp.InvalidInputs, want) {
					t.Fatalf("InvalidInputs = %#v, want %#v", resp.InvalidInputs, want)
				}
			}
		})
	}
}
