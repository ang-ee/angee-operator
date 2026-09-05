package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

func TestUpdateInteractiveRequiresTerminal(t *testing.T) {
	for _, command := range [][]string{
		{"stack", "update", "--template"},
		{"workspace", "update", "feature"},
		{"service", "update", "agent", "--template"},
	} {
		t.Run(command[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
			args := append([]string{"--root", t.TempDir()}, command...)
			args = append(args, "-i")
			cmd.SetArgs(args)
			const want = "--interactive requires a terminal; pass --input/--answers instead"
			if err := cmd.Execute(); err == nil || err.Error() != want {
				t.Fatalf("Execute() error = %v, want %q", err, want)
			}
		})
	}
}

// scriptedUpdateForm uses the real scripted form to exercise command rendering
// without a terminal. Recorded editable values become prompt defaults; accepting
// them still yields no explicit override. Production mode detection is tested
// separately and never permits this fallback for --interactive.
func scriptedUpdateForm(inspect func(inputform.Request)) templateUpdateForm {
	return templateUpdateForm{
		detectMode: func(_ *cobra.Command, yes bool) inputform.Mode {
			return inputform.DetectMode(yes, true, nil)
		},
		run: func(ctx context.Context, req inputform.Request) (inputform.Result, error) {
			if inspect != nil {
				inspect(req)
			}
			req.Mode = inputform.ModeScripted
			for i, desc := range req.Inputs {
				if req.Origins[desc.Name] == inputform.OriginRecorded && desc.Question && !desc.Immutable && !desc.Generated {
					req.Inputs[i].Default = req.Provided[desc.Name]
					delete(req.Provided, desc.Name)
					delete(req.Origins, desc.Name)
				}
			}
			return inputform.Run(ctx, req)
		},
	}
}

func TestStackUpdateInteractiveRerendersChangedAnswer(t *testing.T) {
	t.Setenv("ANGEE_OPERATOR_URL", "")
	root := t.TempDir()
	template := writeInputTemplate(t, root, `_templates_suffix: .jinja
project_name:
  default: original
runtime_mode:
  default: process
  choices: [process, docker]
`)
	if err := os.WriteFile(filepath.Join(template, "angee.yaml.jinja"), []byte("version: 1\nkind: stack\nname: {{ project_name }}\ntemplate:\n  active: stacks/dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "project.txt.jinja"), []byte("{{ project_name }} / {{ runtime_mode }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rendered := filepath.Join(root, "rendered")
	var stdout, stderr bytes.Buffer
	init := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
	init.SetArgs([]string{"--root", root, "stack", "init", "dev", rendered, "--yes"})
	if err := init.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	run := func(answers string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		cmd := NewRootWithIO(strings.NewReader(answers), &stdout, &stderr)
		form := scriptedUpdateForm(func(req inputform.Request) {
			if req.Title != "Update stack from stacks/dev" || req.Confirm != "Re-render the stack?" || req.Mode != inputform.ModeInteractive {
				t.Fatalf("form request: %#v", req)
			}
			for _, key := range []string{"project_name", "runtime_mode"} {
				if req.Origins[key] != inputform.OriginRecorded {
					t.Fatalf("%s origin = %q", key, req.Origins[key])
				}
			}
		})
		cmd.SetContext(context.WithValue(context.Background(), templateUpdateFormKey{}, form))
		cmd.SetArgs([]string{"--root", rendered, "stack", "update", "--template", "-i"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("update: %v; stderr: %s", err, &stderr)
		}
	}
	run("changed\n\n")
	content, err := os.ReadFile(filepath.Join(rendered, "project.txt"))
	if err != nil || string(content) != "changed / process\n" {
		t.Fatalf("rendered project = %q, error = %v", content, err)
	}
	recorded, err := inputform.LoadAnswersFile(filepath.Join(rendered, ".copier-answers.yml"))
	if err != nil || recorded["project_name"] != "changed" || recorded["runtime_mode"] != "process" {
		t.Fatalf("recorded answers = %#v, error = %v", recorded, err)
	}
	if strings.Contains(stderr.String(), "no input changes") {
		t.Fatalf("reported unchanged for an edit: %s", &stderr)
	}
	// A template edit still applies when all recorded inputs are accepted.
	if err := os.WriteFile(filepath.Join(template, "project.txt.jinja"), []byte("new template: {{ project_name }} / {{ runtime_mode }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("\n\n")
	content, err = os.ReadFile(filepath.Join(rendered, "project.txt"))
	if err != nil || string(content) != "new template: changed / process\n" || !strings.Contains(stderr.String(), "no input changes\n") {
		t.Fatalf("unchanged rerender = %q, error = %v, stderr = %q", content, err, stderr.String())
	}
}

type workspaceUpdateInputPlatform struct {
	service.API
	response api.TemplateInputsResponse
	requests []api.WorkspaceUpdateRequest
	names    []string
}

func (p *workspaceUpdateInputPlatform) WorkspaceTemplateInputs(_ context.Context, name string) (api.TemplateInputsResponse, error) {
	p.names = append(p.names, name)
	return p.response, nil
}

func (p *workspaceUpdateInputPlatform) WorkspaceUpdate(_ context.Context, name string, req api.WorkspaceUpdateRequest) (api.WorkspaceRef, error) {
	p.names = append(p.names, name)
	p.requests = append(p.requests, req)
	return api.WorkspaceRef{Name: name}, nil
}

func TestWorkspaceUpdateInteractiveSendsOnlyChangedKeys(t *testing.T) {
	platform := &workspaceUpdateInputPlatform{response: api.TemplateInputsResponse{
		Target: "workspace/feature",
		Template: api.TemplateDescriptor{Ref: "workspaces/dev", Inputs: []api.TemplateInputDescriptor{
			{Name: "topic", Question: true, Default: "template"},
			{Name: "runtime", Question: true, Default: "docker"},
			{Name: "locked", Question: true, Immutable: true},
			{Name: "default_only", Question: true, Default: "renderer"},
		}},
		Recorded: map[string]string{"topic": "recorded", "runtime": "process", "locked": "fixed"},
	}}
	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader("changed\n\n\n"), &stdout, &stderr)
	cmd.SetContext(context.WithValue(context.Background(), templateUpdateFormKey{}, scriptedUpdateForm(func(req inputform.Request) {
		if req.Title != "Update workspace feature from workspaces/dev" || req.Confirm != "Re-render the workspace?" || !req.Inputs[2].Immutable {
			t.Fatalf("form request = %#v", req)
		}
	})))
	ref, err := updateWorkspaceFromTemplate(cmd, platform, "feature", api.WorkspaceUpdateRequest{TTL: "2h", Overwrite: true}, true)
	if err != nil || ref.Name != "feature" {
		t.Fatalf("update = %#v, error = %v", ref, err)
	}
	if len(platform.requests) != 1 || !reflect.DeepEqual(platform.requests[0], api.WorkspaceUpdateRequest{Inputs: map[string]string{"topic": "changed"}, TTL: "2h", Overwrite: true}) || !reflect.DeepEqual(platform.names, []string{"feature", "feature"}) {
		t.Fatalf("requests = %#v, names = %#v", platform.requests, platform.names)
	}
	if !reflect.DeepEqual(platform.response.Recorded, map[string]string{"topic": "recorded", "runtime": "process", "locked": "fixed"}) {
		t.Fatalf("mutated recorded answers: %#v", platform.response.Recorded)
	}
}

func TestUpdateFormLayersAnswersAndFlagsAndClearsUnrecordedSecrets(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
	cmd.Flags().StringArray("answers", nil, "")
	for i, contents := range []string{"answer: first\nflag: first\n", "answer: second\nflag: second\n"} {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("answers%d.yml", i))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("answers", path); err != nil {
			t.Fatal(err)
		}
	}
	response := api.TemplateInputsResponse{
		Template: api.TemplateDescriptor{Ref: "services/agent", Inputs: []api.TemplateInputDescriptor{
			{Name: "recorded", Question: true},
			{Name: "answer", Question: true},
			{Name: "flag", Question: true},
			{Name: "secret", Question: true, Secret: true, Default: "hidden-default", Help: "API credential"},
			{Name: "locked", Question: true, Secret: true, Immutable: true, Default: "hidden-fixed"},
		}},
		Recorded: map[string]string{"recorded": "old", "answer": "old", "flag": "old"}, Unrecorded: []string{"secret", "locked"},
	}
	form := templateUpdateForm{run: func(_ context.Context, req inputform.Request) (inputform.Result, error) {
		if req.Title != "Update service agent from services/agent" || req.Confirm != "Re-render the service?" {
			t.Fatalf("form request = %#v", req)
		}
		want := map[string]string{"recorded": "old", "answer": "second", "flag": "flag"}
		origins := map[string]inputform.Origin{"recorded": inputform.OriginRecorded, "answer": inputform.OriginAnswers, "flag": inputform.OriginFlag}
		if !reflect.DeepEqual(req.Provided, want) || !reflect.DeepEqual(req.Origins, origins) {
			t.Fatalf("provided = %#v; origins = %#v", req.Provided, req.Origins)
		}
		if req.Inputs[3].Default != "" || req.Inputs[3].Help != "API credential\n(not recorded)" || !req.Inputs[3].Secret || req.Inputs[3].Immutable || !req.Inputs[3].Question {
			t.Fatalf("unrecorded secret = %#v", req.Inputs[3])
		}
		if !req.Inputs[4].Immutable || req.Inputs[4].Default != "" {
			t.Fatalf("immutable unrecorded secret = %#v", req.Inputs[4])
		}
		return inputform.Result{Values: req.Provided, Origins: req.Origins}, nil
	}}
	cmd.SetContext(context.WithValue(context.Background(), templateUpdateFormKey{}, form))
	got, err := resolveUpdateTemplateInputs(cmd, map[string]string{"flag": "flag"}, true, "service", "agent", func(context.Context) (api.TemplateInputsResponse, error) {
		return response, nil
	})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"answer": "second", "flag": "flag"}) {
		t.Fatalf("inputs = %#v, error = %v", got, err)
	}
	if response.Template.Inputs[3].Default != "hidden-default" || response.Template.Inputs[3].Help != "API credential" {
		t.Fatalf("mutated descriptor = %#v", response.Template.Inputs[3])
	}
}

func TestUpdateAnswersWithoutInteractive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
	cmd.Flags().StringArray("answers", nil, "")
	path := filepath.Join(t.TempDir(), "answers.yml")
	if err := os.WriteFile(path, []byte("from_file: answer\nflag: file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("answers", path); err != nil {
		t.Fatal(err)
	}
	got, err := resolveUpdateTemplateInputs(cmd, map[string]string{"flag": "flag"}, false, "stack", "", func(context.Context) (api.TemplateInputsResponse, error) {
		t.Fatal("non-interactive update fetched a descriptor")
		return api.TemplateInputsResponse{}, nil
	})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"from_file": "answer", "flag": "flag"}) || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("inputs = %#v, error = %v, stdout = %q, stderr = %q", got, err, stdout.String(), stderr.String())
	}
}

func TestUpdateFormAbortDoesNotUpdateWorkspace(t *testing.T) {
	platform := &workspaceUpdateInputPlatform{}
	cmd := NewRootWithIO(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	cmd.SetContext(context.WithValue(context.Background(), templateUpdateFormKey{}, templateUpdateForm{
		run: func(context.Context, inputform.Request) (inputform.Result, error) {
			return inputform.Result{}, inputform.ErrAborted
		},
	}))
	_, err := updateWorkspaceFromTemplate(cmd, platform, "feature", api.WorkspaceUpdateRequest{}, true)
	if !errors.Is(err, inputform.ErrAborted) || len(platform.requests) != 0 {
		t.Fatalf("error = %v, updates = %#v", err, platform.requests)
	}
}
