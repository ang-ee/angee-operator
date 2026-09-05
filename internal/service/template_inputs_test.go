package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

const updateInputQuestions = `
label:
  type: str
  default: initial
locked:
  type: str
  default: original
  immutable: true
inherited:
  type: str
  default: fallback
token:
  type: str
  default: ""
  secret: true
locked_secret:
  type: str
  default: ""
  secret: true
  immutable: true
`

func appendUpdateInputQuestions(t *testing.T, template string) {
	t.Helper()
	path := filepath.Join(template, "copier.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(updateInputQuestions)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStackTemplateInputsFixture(t *testing.T) (*Platform, string) {
	t.Helper()
	project := t.TempDir()
	body := strings.ReplaceAll(oneServiceTemplate, "nginx:latest", "nginx:{{ label }}")
	body = strings.ReplaceAll(body, ".copier-answers.yml", ".copier-answers.stack.yml")
	template := writeStackTemplate(t, project, body)
	appendUpdateInputQuestions(t, template)
	path := filepath.Join(template, "copier.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n_answers_file: .copier-answers.stack.yml\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	platform, err := New(project)
	if err != nil {
		t.Fatal(err)
	}
	created, err := platform.StackInit(context.Background(), "dev", "", map[string]string{
		"label": "before", "locked": "original", "token": "hidden-token", "locked_secret": "hidden-locked",
	}, false)
	if err != nil {
		t.Fatalf("StackInit: %v", err)
	}
	platform, err = New(created.Root)
	if err != nil {
		t.Fatal(err)
	}
	return platform, template
}

func newWorkspaceTemplateInputsFixture(t *testing.T) (*Platform, string) {
	t.Helper()
	root := t.TempDir()
	template := filepath.Join(root, ".templates", "workspaces", "simple")
	if err := os.MkdirAll(filepath.Join(template, "template"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte("_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: workspace\n  name: simple\n"+updateInputQuestions), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "answers.txt.jinja"), []byte("{{ label }}/{{ inherited }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.WorkspaceCreate(context.Background(), api.WorkspaceCreateRequest{
		Template: "simple", Name: "feature", Inputs: map[string]string{"label": "before", "locked": "original"},
	}); err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	stack, err := platform.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.WorkspaceDefaults = map[string]manifest.WorkspaceDefaults{
		"simple": {Inputs: map[string]string{"label": "stack-default", "inherited": "stack-inherited", "_metadata": "drop"}},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatal(err)
	}
	return platform, template
}

func newServiceTemplateInputsFixture(t *testing.T) (*Platform, string) {
	t.Helper()
	platform, fixture := setupServiceCreateFixture(t)
	template := copyServiceTemplateFixture(t, fixture)
	appendUpdateInputQuestions(t, template)
	if err := os.WriteFile(filepath.Join(template, "template", "answers.txt.jinja"), []byte("{{ label }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ServiceCreate(context.Background(), api.ServiceCreateRequest{
		Template: template, Workspace: "my-pa", Inputs: map[string]string{
			"label": "before", "locked": "original", "token": "hidden-token", "locked_secret": "hidden-locked",
		},
	}); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}
	return platform, template
}

func assertTemplateInputs(t *testing.T, response api.TemplateInputsResponse, target, ref, template string) {
	t.Helper()
	if response.Target != target || response.Template.Ref != ref || response.Template.Path != template {
		t.Fatalf("target/template = %q/%#v, want %q/%q/%q", response.Target, response.Template, target, ref, template)
	}
	if response.Recorded["label"] != "before" || response.Recorded["locked"] != "original" {
		t.Fatalf("recorded = %#v", response.Recorded)
	}
	for key := range response.Recorded {
		if strings.HasPrefix(key, "_") {
			t.Fatalf("recorded contains metadata key %q", key)
		}
	}
	if !reflect.DeepEqual(response.Unrecorded, []string{"token", "locked_secret"}) {
		t.Fatalf("unrecorded = %#v", response.Unrecorded)
	}
}

func TestStackTemplateInputsRenderedFixture(t *testing.T) {
	ctx := context.Background()
	platform, template := newStackTemplateInputsFixture(t)
	response, err := platform.StackTemplateInputs(ctx)
	if err != nil {
		t.Fatalf("StackTemplateInputs: %v", err)
	}
	assertTemplateInputs(t, response, "stack", "stacks/dev", template)
	if _, err := platform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{"label": "after", "locked": "original"}}); err != nil {
		t.Fatalf("StackUpdateFromTemplate: %v", err)
	}
	stack, err := platform.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	if stack.Services["web"].Image != "nginx:after" {
		t.Fatalf("rendered image = %q", stack.Services["web"].Image)
	}
	// The same parent answers file and _src_path fallback remain usable when
	// a legacy manifest no longer carries template.active.
	stack.Template.Active = ""
	if err := manifest.SaveFile(manifest.Path(platform.root), stack); err != nil {
		t.Fatal(err)
	}
	response, err = platform.StackTemplateInputs(ctx)
	if err != nil {
		t.Fatalf("StackTemplateInputs fallback: %v", err)
	}
	if response.Template.Ref != template || response.Recorded["label"] != "after" {
		t.Fatalf("fallback response = %#v", response)
	}
}

func TestWorkspaceTemplateInputsRenderedFixture(t *testing.T) {
	ctx := context.Background()
	platform, template := newWorkspaceTemplateInputsFixture(t)
	response, err := platform.WorkspaceTemplateInputs(ctx, "feature")
	if err != nil {
		t.Fatalf("WorkspaceTemplateInputs: %v", err)
	}
	assertTemplateInputs(t, response, "workspace/feature", "workspaces/simple", template)
	if response.Recorded["inherited"] != "stack-inherited" {
		t.Fatalf("stack defaults missing: %#v", response.Recorded)
	}
	if _, err := platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{Inputs: map[string]string{"label": "after", "locked": "original"}}); err != nil {
		t.Fatalf("WorkspaceUpdate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(platform.root, "workspaces", "feature", "answers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "after/stack-inherited\n" {
		t.Fatalf("rendered answers = %q", data)
	}
	response, err = platform.WorkspaceTemplateInputs(ctx, "feature")
	if err != nil || response.Recorded["label"] != "after" {
		t.Fatalf("updated response = %#v, err = %v", response, err)
	}
}

func TestServiceTemplateInputsRenderedFixture(t *testing.T) {
	ctx := context.Background()
	platform, template := newServiceTemplateInputsFixture(t)
	response, err := platform.ServiceTemplateInputs(ctx, "agent-my-pa")
	if err != nil {
		t.Fatalf("ServiceTemplateInputs: %v", err)
	}
	assertTemplateInputs(t, response, "service/agent-my-pa", template, template)
	if _, err := platform.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{Inputs: map[string]string{"label": "after", "locked": "original"}}); err != nil {
		t.Fatalf("ServiceUpdateFromTemplate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(platform.root, "services", "agent-my-pa", "answers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "after\n" {
		t.Fatalf("rendered answers = %q", data)
	}
	response, err = platform.ServiceTemplateInputs(ctx, "agent-my-pa")
	if err != nil || response.Recorded["label"] != "after" {
		t.Fatalf("updated response = %#v, err = %v", response, err)
	}
	// Legacy services without render state use the answers file's _src_path.
	if err := os.Remove(renderPlanStatePath(platform.root, "services", "agent-my-pa")); err != nil {
		t.Fatal(err)
	}
	response, err = platform.ServiceTemplateInputs(ctx, "agent-my-pa")
	if err != nil || response.Template.Ref != template {
		t.Fatalf("legacy response = %#v, err = %v", response, err)
	}
}

func TestTemplateUpdatesRejectImmutableInputs(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"stack", "workspace", "service"} {
		t.Run(kind, func(t *testing.T) {
			var update func(map[string]string) error
			switch kind {
			case "stack":
				platform, _ := newStackTemplateInputsFixture(t)
				update = func(inputs map[string]string) error {
					_, err := platform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: inputs})
					return err
				}
			case "workspace":
				platform, _ := newWorkspaceTemplateInputsFixture(t)
				update = func(inputs map[string]string) error {
					_, err := platform.WorkspaceUpdate(ctx, "feature", api.WorkspaceUpdateRequest{Inputs: inputs})
					return err
				}
			case "service":
				platform, _ := newServiceTemplateInputsFixture(t)
				update = func(inputs map[string]string) error {
					_, err := platform.ServiceUpdateFromTemplate(ctx, "agent-my-pa", api.ServiceUpdateTemplateRequest{Inputs: inputs})
					return err
				}
			}
			for _, key := range []string{"locked", "locked_secret"} {
				err := update(map[string]string{key: "replacement-secret"})
				var invalid *InvalidInputError
				if !errors.As(err, &invalid) || invalid.Field != key {
					t.Fatalf("update %s error = %v, want InvalidInputError naming %s", key, err, key)
				}
				want := `template input locked is immutable; it was rendered as "original"`
				if key == "locked_secret" {
					want = "template input locked_secret is immutable; it cannot change after the first render"
				}
				if invalid.Reason != want {
					t.Fatalf("immutable reason = %q, want %q", invalid.Reason, want)
				}
			}
		})
	}
}

func TestStackTemplateInputsOriginErrorsMatchUpdate(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []string{"answers", "template"} {
		t.Run(missing, func(t *testing.T) {
			platform, _ := newStackTemplateInputsFixture(t)
			answersPath := filepath.Join(filepath.Dir(platform.root), ".copier-answers.stack.yml")
			if missing == "answers" {
				if err := os.Remove(answersPath); err != nil {
					t.Fatal(err)
				}
			} else {
				stack, err := platform.LoadStack()
				if err != nil {
					t.Fatal(err)
				}
				stack.Template.Active = ""
				if err := manifest.SaveFile(manifest.Path(platform.root), stack); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(answersPath, []byte("ANGEE_ROOT: .angee\nlabel: before\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, readErr := platform.StackTemplateInputs(ctx)
			_, updateErr := platform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
			var readInvalid, updateInvalid *InvalidInputError
			if !errors.As(readErr, &readInvalid) || !errors.As(updateErr, &updateInvalid) || !reflect.DeepEqual(readInvalid, updateInvalid) {
				t.Fatalf("read error %v != update error %v", readErr, updateErr)
			}
		})
	}
}

func TestReadTemplateAnswersPreservesFormValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "answers.yml")
	data := "_src_path: /template\n_commit: abc\ntext: 00123\ndate: 2026-09-05\nlarge: 12345678901234567890\nempty: null\nchoices: [one, two]\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	source, answers, err := readTemplateAnswers(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"text": "00123", "date": "2026-09-05", "large": "12345678901234567890", "empty": "", "choices": `["one","two"]`}
	if source != "/template" || !reflect.DeepEqual(map[string]string(answers), want) {
		t.Fatalf("source/answers = %q/%#v", source, answers)
	}
}
