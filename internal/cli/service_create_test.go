package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

type serviceCreateInputsPlatform struct {
	service.API
	descriptor api.TemplateDescriptor
	refs       []string
	requests   []api.ServiceCreateRequest
}

func (p *serviceCreateInputsPlatform) Template(_ context.Context, ref string) (api.TemplateDescriptor, error) {
	p.refs = append(p.refs, ref)
	return p.descriptor, nil
}

func (p *serviceCreateInputsPlatform) ServiceCreate(_ context.Context, req api.ServiceCreateRequest) (api.ServiceState, error) {
	p.requests = append(p.requests, req)
	return api.ServiceState{Name: "worker", Runtime: "local", Status: "stopped"}, nil
}

func TestServiceCreateSendsOnlyExplicitInputs(t *testing.T) {
	answers := filepath.Join(t.TempDir(), "answers.yml")
	if err := os.WriteFile(answers, []byte("_src_path: services/worker\ncount: 4\nruntime: process\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "scripted", true: "yes"}[yes], func(t *testing.T) {
			platform := &serviceCreateInputsPlatform{descriptor: api.TemplateDescriptor{Inputs: []api.TemplateInputDescriptor{
				{Name: "project", Question: true, Required: true},
				{Name: "runtime", Question: true, Default: "process"},
				{Name: "count", Question: true, Type: "int", Default: "1"},
				{Name: "size", Question: true, Default: "small"},
			}}}
			var stderr bytes.Buffer
			cmd := serviceInputTestCommand("", &stderr)
			if err := cmd.Flags().Set("answers", answers); err != nil {
				t.Fatal(err)
			}
			req := api.ServiceCreateRequest{Template: "worker", Workspace: "dev", Inputs: map[string]string{
				"project": "demo", "runtime": "docker", "extra": "keep",
			}}
			if _, err := createServiceFromTemplate(cmd, platform, req, yes, false); err != nil {
				t.Fatalf("create service: %v", err)
			}
			if len(platform.requests) != 1 {
				t.Fatalf("create requests = %d, want 1", len(platform.requests))
			}
			want := map[string]string{"project": "demo", "runtime": "docker", "count": "4", "extra": "keep"}
			if got := platform.requests[0].Inputs; !reflect.DeepEqual(got, want) {
				t.Fatalf("request inputs = %#v, want %#v", got, want)
			}
			if !reflect.DeepEqual(platform.refs, []string{"services/worker"}) {
				t.Fatalf("descriptor refs = %#v", platform.refs)
			}
			if stderr.Len() != 0 {
				t.Fatalf("satisfied inputs printed prompts: %q", stderr.String())
			}
		})
	}
}

func TestServiceCreateScriptedPromptsOnlyForMissingInputs(t *testing.T) {
	platform := &serviceCreateInputsPlatform{descriptor: api.TemplateDescriptor{Inputs: []api.TemplateInputDescriptor{
		{Name: "runtime", Question: true, Default: "process"},
		{Name: "token", Question: true, Required: true},
		{Name: "optional", Question: true},
	}}}
	var stderr bytes.Buffer
	cmd := serviceInputTestCommand("secret-value\n", &stderr)
	if _, err := createServiceFromTemplate(cmd, platform, api.ServiceCreateRequest{Template: "services/worker", Workspace: "dev"}, false, false); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if len(platform.requests) != 1 || !reflect.DeepEqual(platform.requests[0].Inputs, map[string]string{"token": "secret-value"}) {
		t.Fatalf("requests = %#v", platform.requests)
	}
	if got := stderr.String(); strings.Count(got, "token: ") != 1 || strings.Contains(got, "runtime") || strings.Contains(got, "optional") {
		t.Fatalf("prompts = %q, want exactly one token prompt", got)
	}
}

func TestServiceCreateAnswersInvalidChoiceBeforePrompts(t *testing.T) {
	answers := filepath.Join(t.TempDir(), "answers.yml")
	if err := os.WriteFile(answers, []byte("runtime: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "scripted", true: "yes"}[yes], func(t *testing.T) {
			platform := &serviceCreateInputsPlatform{descriptor: api.TemplateDescriptor{Inputs: []api.TemplateInputDescriptor{
				{Name: "token", Question: true, Required: true},
				{Name: "runtime", Question: true, Choices: []api.TemplateInputChoice{{Value: "process"}, {Value: "docker"}}},
			}}}
			var stderr bytes.Buffer
			cmd := serviceInputTestCommand("", &stderr)
			if err := cmd.Flags().Set("answers", answers); err != nil {
				t.Fatal(err)
			}
			_, err := createServiceFromTemplate(cmd, platform, api.ServiceCreateRequest{Template: "worker", Workspace: "dev"}, yes, false)
			if err == nil || err.Error() != "template input runtime must be one of: process, docker" {
				t.Fatalf("error = %v", err)
			}
			if stderr.Len() != 0 || len(platform.requests) != 0 {
				t.Fatalf("invalid answers prompted or created: stderr=%q requests=%#v", stderr.String(), platform.requests)
			}
		})
	}
}

func TestServiceCreateTemplateRefs(t *testing.T) {
	for _, ref := range []string{"worker", "services/worker", "https://example.com/service.git", "/local/template", "../local/template"} {
		t.Run(ref, func(t *testing.T) {
			platform := &serviceCreateInputsPlatform{}
			var stderr bytes.Buffer
			cmd := serviceInputTestCommand("", &stderr)
			req := api.ServiceCreateRequest{Template: ref, Workspace: "dev", Inputs: map[string]string{"extra": "value"}}
			if _, err := createServiceFromTemplate(cmd, platform, req, true, false); err != nil {
				t.Fatalf("create service: %v", err)
			}
			if filepath.IsAbs(ref) || strings.Contains(ref, "..") {
				if len(platform.refs) != 0 {
					t.Fatalf("path template fetched descriptor: %#v", platform.refs)
				}
			} else {
				want := ref
				if ref == "worker" {
					want = "services/worker"
				}
				if !reflect.DeepEqual(platform.refs, []string{want}) {
					t.Fatalf("descriptor refs = %#v, want %q", platform.refs, want)
				}
			}
			if len(platform.requests) != 1 || !reflect.DeepEqual(platform.requests[0], req) {
				t.Fatalf("requests = %#v, want %#v", platform.requests, req)
			}
		})
	}
}

func TestServiceCreateRejectsInteractiveWithYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
	cmd.SetArgs([]string{"--root", t.TempDir(), "service", "create", "--template", "worker", "--workspace", "dev", "-i", "--yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--interactive") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v, want conflicting flags", err)
	}
}

func TestTemplateInputProblems(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desc    api.TemplateInputDescriptor
		inputs  map[string]string
		missing bool
		invalid bool
	}{
		{name: "required metadata", desc: api.TemplateInputDescriptor{Name: "name", Required: true}, missing: true},
		{name: "required default", desc: api.TemplateInputDescriptor{Name: "name", Required: true, Default: "dev"}},
		{name: "required empty flag overrides default", desc: api.TemplateInputDescriptor{Name: "name", Required: true, Default: "dev"}, inputs: map[string]string{"name": ""}, missing: true},
		{name: "required generated", desc: api.TemplateInputDescriptor{Name: "name", Generated: true, Required: true}},
		{name: "generated explicit invalid", desc: api.TemplateInputDescriptor{Name: "name", Generated: true, Type: "int"}, inputs: map[string]string{"name": "bad"}, invalid: true},
		{name: "unanswered optional integer", desc: api.TemplateInputDescriptor{Name: "name", Type: "int"}},
		{name: "invalid default", desc: api.TemplateInputDescriptor{Name: "name", Type: "int", Default: "bad"}, invalid: true},
		{name: "required multiselect", desc: api.TemplateInputDescriptor{Name: "name", Multiselect: true, Required: true}, missing: true},
		{name: "empty explicit multiselect", desc: api.TemplateInputDescriptor{Name: "name", Multiselect: true, Required: true}, inputs: map[string]string{"name": "[ ]"}, missing: true},
		{name: "invalid multiselect", desc: api.TemplateInputDescriptor{Name: "name", Multiselect: true, Required: true}, inputs: map[string]string{"name": "null"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, invalid, err := templateInputProblems([]api.TemplateInputDescriptor{tc.desc}, tc.inputs)
			if missing != tc.missing || invalid != tc.invalid || (err != nil) != (tc.missing || tc.invalid) {
				t.Fatalf("problems = (%t, %t, %v), want (%t, %t)", missing, invalid, err, tc.missing, tc.invalid)
			}
		})
	}
}

func serviceInputTestCommand(stdin string, stderr *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(stderr)
	cmd.Flags().StringArray("answers", nil, "answers files")
	return cmd
}
