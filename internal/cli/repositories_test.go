package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/spf13/cobra"
)

// repositoryInputTemplate is a stack template that declares both repository
// list inputs and renders one source per entry of each.
const repositoryInputTemplate = `_answers_file: .copier-answers.stack.yml
repositories:
  type: json
  default: "[]"
extra_sources:
  type: json
  default: "[]"
`

const repositoryInputManifest = `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.stack.yml
sources:
{% for repository in repositories %}  {{ repository.name }}:
    kind: git
    repo: {{ repository.repo|to_json }}
    default_ref: {{ repository.ref|default:"main"|to_json }}
    slot: true
{% endfor %}{% for repository in extra_sources %}  {{ repository.name }}:
    kind: git
    repo: {{ repository.repo|to_json }}
{% endfor %}services:
  web:
    runtime: container
    image: nginx:latest
`

// repositoryFlagCommand is a command with the repository flags, parsed from
// args.
func repositoryFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "init", RunE: func(*cobra.Command, []string) error { return nil }}
	addRepositoryFlags(cmd, "")
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v", args, err)
	}
	return cmd
}

// Each flag packs the strings it was given, in order and unread, into its
// template input as a JSON array; the service reads them.
func TestRepositoryFlagsPackIntoTemplateInputs(t *testing.T) {
	cmd := repositoryFlagCommand(t,
		"--repository", "git@github.com:phro-gg/angee-omelas.git",
		"--extra-source", "ang-ee/angee-arp@feature/x",
		"--repository", "not parsed here, whatever it is",
	)
	inputs, err := repositoryFlagInputs(cmd, map[string]string{"project_name": "omelas"})
	if err != nil {
		t.Fatalf("repositoryFlagInputs() error = %v", err)
	}
	want := map[string]string{
		"project_name":  "omelas",
		"repositories":  `["git@github.com:phro-gg/angee-omelas.git","not parsed here, whatever it is"]`,
		"extra_sources": `["ang-ee/angee-arp@feature/x"]`,
	}
	if len(inputs) != len(want) {
		t.Fatalf("inputs = %v, want %v", inputs, want)
	}
	for key, value := range want {
		if inputs[key] != value {
			t.Fatalf("input %s = %q, want %q", key, inputs[key], value)
		}
	}

	// No flag: the inputs are as given, a nil map included.
	if inputs, err := repositoryFlagInputs(repositoryFlagCommand(t), nil); err != nil || inputs != nil {
		t.Fatalf("repositoryFlagInputs(no flags) = %v, %v; want nil", inputs, err)
	}
	if inputs, err := repositoryFlagInputs(repositoryFlagCommand(t, "--repository", "a/b"), nil); err != nil || inputs["repositories"] != `["a/b"]` {
		t.Fatalf("repositoryFlagInputs(nil inputs) = %v, %v", inputs, err)
	}
}

// A flag and `--input` for the same input would each give the whole list.
func TestRepositoryFlagRefusesItsInputGivenToo(t *testing.T) {
	for _, tc := range []struct {
		flag, input, want string
	}{
		{"--repository", "repositories", `--repository and --input repositories=… both give the template input "repositories"; give the list one way`},
		{"--extra-source", "extra_sources", `--extra-source and --input extra_sources=… both give the template input "extra_sources"; give the list one way`},
	} {
		cmd := repositoryFlagCommand(t, tc.flag, "ang-ee/angee-arp")
		if _, err := repositoryFlagInputs(cmd, map[string]string{tc.input: "[]"}); err == nil || err.Error() != tc.want {
			t.Fatalf("repositoryFlagInputs(%s with --input %s) error = %v, want %q", tc.flag, tc.input, err, tc.want)
		}
		// The other list's input is no conflict.
		other := map[string]string{"repositories": "extra_sources", "extra_sources": "repositories"}[tc.input]
		if _, err := repositoryFlagInputs(cmd, map[string]string{other: "[]"}); err != nil {
			t.Fatalf("repositoryFlagInputs(%s with --input %s) error = %v", tc.flag, other, err)
		}
	}
}

// `angee init` and `angee stack init` take the flags and render one source
// per repository; the answers record the canonical lists.
func TestInitTakesRepositoryFlags(t *testing.T) {
	for _, command := range [][]string{{"init", "--template", "dev"}, {"stack", "init", "dev"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			root := t.TempDir()
			template := writeInputTemplate(t, root, repositoryInputTemplate)
			if err := os.WriteFile(filepath.Join(template, "angee.yaml.jinja"), []byte(repositoryInputManifest), 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "stack")
			var stdout, stderr bytes.Buffer
			cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
			cmd.SetArgs(append(append([]string{"--root", root}, command...), target, "--yes",
				"--repository", "git@github.com:phro-gg/angee-omelas.git",
				"--repository", "ang-ee/angee-odoo@feature/arp-ap",
				"--extra-source", "git@github.com:ang-ee/angee-arp.git"))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v; stderr: %s", err, &stderr)
			}
			stack, err := manifest.LoadFile(manifest.Path(target))
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			for name, want := range map[string]manifest.Source{
				"angee-omelas": {Kind: "git", Repo: "git@github.com:phro-gg/angee-omelas.git", DefaultRef: "main", Slot: true},
				"angee-odoo":   {Kind: "git", Repo: "https://github.com/ang-ee/angee-odoo.git", DefaultRef: "feature/arp-ap", Slot: true},
				"angee-arp":    {Kind: "git", Repo: "git@github.com:ang-ee/angee-arp.git"},
			} {
				got := stack.Sources[name]
				if got.Kind != want.Kind || got.Repo != want.Repo || got.DefaultRef != want.DefaultRef || got.Slot != want.Slot {
					t.Fatalf("source %s = %+v, want %+v", name, got, want)
				}
			}
			if len(stack.Sources) != 3 {
				t.Fatalf("sources = %v, want three", stack.Sources)
			}
			// The answers file reads back as the lists the service made of
			// the flags: sorted by name, each entry with its name and ssh.
			answers, err := inputform.LoadAnswersFile(filepath.Join(target, ".copier-answers.stack.yml"))
			if err != nil {
				t.Fatal(err)
			}
			wantRepositories := `[{"name":"angee-odoo","ref":"feature/arp-ap","repo":"https://github.com/ang-ee/angee-odoo.git","ssh":false},{"name":"angee-omelas","ref":"","repo":"git@github.com:phro-gg/angee-omelas.git","ssh":true}]`
			if answers["repositories"] != wantRepositories {
				t.Fatalf("recorded repositories =\n%s\nwant\n%s", answers["repositories"], wantRepositories)
			}

			// The recorded answers initialize a second stack of the same lists.
			second := filepath.Join(root, "second")
			cmd = NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
			cmd.SetArgs(append(append([]string{"--root", root}, command...), second, "--yes", "--answers", filepath.Join(target, ".copier-answers.stack.yml")))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(--answers) error = %v; stderr: %s", err, &stderr)
			}
			replayed, err := os.ReadFile(filepath.Join(second, "angee.yaml"))
			first, _ := os.ReadFile(filepath.Join(target, "angee.yaml"))
			if err != nil || string(replayed) != string(first) {
				t.Fatalf("stack from the recorded answers =\n%s\nwant\n%s\n(%v)", replayed, first, err)
			}
		})
	}
}

func TestInitRefusesRepositoryFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		questions string
		args      []string
		want      string
	}{
		{
			name:      "a flag and its input",
			questions: repositoryInputTemplate,
			args:      []string{"--repository", "ang-ee/angee-arp", "--input", `repositories=["ang-ee/angee-odoo"]`},
			want:      `--repository and --input repositories=… both give the template input "repositories"; give the list one way`,
		},
		{
			name:      "a flag the template has no input for",
			questions: "project_name:\n  default: app\n",
			args:      []string{"--extra-source", "ang-ee/angee-arp"},
			want:      `extra_sources: stack template "dev" declares no input "extra_sources", so it takes no --extra-source`,
		},
		{
			name:      "a repository that is none",
			questions: repositoryInputTemplate,
			args:      []string{"--repository", "angee-arp"},
			want:      `repositories: entry 1: "angee-arp" is not a repository`,
		},
		{
			name:      "a repository in both lists",
			questions: repositoryInputTemplate,
			args:      []string{"--repository", "ang-ee/angee-arp", "--extra-source", "git@github.com:ang-ee/angee-arp.git"},
			want:      `extra_sources: repository "angee-arp" is in both repositories and extra_sources`,
		},
	} {
		for _, command := range [][]string{{"init", "--template", "dev"}, {"stack", "init", "dev"}} {
			t.Run(tc.name+"/"+strings.Join(command, " "), func(t *testing.T) {
				root := t.TempDir()
				template := writeInputTemplate(t, root, tc.questions)
				if err := os.WriteFile(filepath.Join(template, "angee.yaml.jinja"), []byte("version: 1\nkind: stack\nname: demo\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "stack")
				var stdout, stderr bytes.Buffer
				cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
				cmd.SetArgs(append(append(append([]string{"--root", root}, command...), target, "--yes"), tc.args...))
				err := cmd.Execute()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Execute() error = %v, want %q", err, tc.want)
				}
				if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
					t.Fatalf("stack root after a refused init: %v, want nothing rendered", statErr)
				}
			})
		}
	}
}

// The flags replace a recorded list on `stack update --template`, and mean
// nothing without it.
func TestStackUpdateTakesRepositoryFlags(t *testing.T) {
	root := t.TempDir()
	template := writeInputTemplate(t, root, repositoryInputTemplate)
	if err := os.WriteFile(filepath.Join(template, "angee.yaml.jinja"), []byte(repositoryInputManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "stack")
	run := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return stdout.String(), err
	}
	if _, err := run("--root", root, "init", "--template", "dev", target, "--yes", "--repository", "ang-ee/angee-odoo"); err != nil {
		t.Fatalf("init error = %v", err)
	}
	t.Chdir(root) // the update resolves template `stacks/dev` from here
	if out, err := run("--root", target, "stack", "update", "--template", "--repository", "ang-ee/angee-odoo"); err != nil || !strings.Contains(out, "stack template up to date") {
		t.Fatalf("update with the recorded list = %q, %v; want no change", out, err)
	}
	out, err := run("--root", target, "stack", "update", "--template", "--repository", "ang-ee/angee-odoo", "--repository", "ang-ee/angee-bridges@next")
	if err != nil || !strings.Contains(out, "+ sources/angee-bridges") {
		t.Fatalf("update with a longer list = %q, %v; want the new source", out, err)
	}
	stack, err := manifest.LoadFile(manifest.Path(target))
	if err != nil || stack.Sources["angee-bridges"].DefaultRef != "next" || !stack.Sources["angee-bridges"].Slot {
		t.Fatalf("sources after the update = %+v, %v", stack.Sources, err)
	}
	if _, err := run("--root", target, "stack", "update", "--repository", "ang-ee/angee-odoo"); err == nil || err.Error() != "--repository and --extra-source only apply with --template" {
		t.Fatalf("update without --template error = %v", err)
	}
	if _, err := run("--root", target, "stack", "update", "--template", "--extra-source", "x/y", "--input", "extra_sources=[]"); err == nil || !strings.Contains(err.Error(), "--extra-source and --input extra_sources=… both give") {
		t.Fatalf("update with a flag and its input error = %v", err)
	}
}
