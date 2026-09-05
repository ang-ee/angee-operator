package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

func TestVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "angee version " + Version + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "aborted", err: inputform.ErrAborted, want: 130},
		{name: "wrapped abort", err: fmt.Errorf("form: %w", inputform.ErrAborted), want: 130},
		{name: "ordinary error", err: errors.New("failed"), want: 1},
		{name: "same text", err: errors.New(inputform.ErrAborted.Error()), want: 1},
		{name: "nil", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "angee version " + Version + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestVersionCommandJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", "version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "{\n  \"version\": \"" + Version + "\"\n}\n"
	if got := stdout.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestVerboseFlagInstallsDebugLogger(t *testing.T) {
	t.Setenv("ANGEE_VERBOSE", "0")
	assertLoggerLevel(t, []string{"-vv", "logger-level-test"}, slog.LevelDebug)
}

func TestVerboseEnvInstallsInfoLogger(t *testing.T) {
	t.Setenv("ANGEE_VERBOSE", "1")
	assertLoggerLevel(t, []string{"logger-level-test"}, slog.LevelInfo)
}

func assertLoggerLevel(t *testing.T, args []string, level slog.Level) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	var enabled bool
	var tooVerbose bool
	cmd.AddCommand(&cobra.Command{
		Use:    "logger-level-test",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := logctx.From(cmd.Context())
			enabled = logger.Enabled(cmd.Context(), level)
			tooVerbose = logger.Enabled(cmd.Context(), level-1)
			return nil
		},
	})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !enabled {
		t.Fatalf("logger is not enabled at %s", level)
	}
	if tooVerbose {
		t.Fatalf("logger is unexpectedly enabled below %s", level)
	}
}

func TestServiceUpdateTemplateRejectsFieldFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"service", "update", "agent", "--template", "--image", "local:latest"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--image cannot be combined with --template") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestServiceUpdateOverwriteRequiresTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"service", "update", "agent", "--overwrite"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "only apply with --template") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestServiceUpdateTemplateJSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/services/agent/template/update" {
			t.Fatalf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		_ = json.NewEncoder(w).Encode(api.ServiceTemplateUpdateResult{
			Name:    "agent",
			Changed: true,
			Changes: []api.TemplateChange{{Path: "AGENTS.md", Kind: "modify"}},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--operator", server.URL, "--json", "service", "update", "agent", "--template", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var result api.ServiceTemplateUpdateResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(output): %v; output = %q", err, stdout.String())
	}
	if result.Name != "agent" || !result.Changed || len(result.Changes) != 1 || result.Changes[0].Path != "AGENTS.md" {
		t.Fatalf("result = %#v", result)
	}
}

func TestServiceUpdateTemplateDryRunConflictOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ServiceTemplateUpdateResult{
			Name:      "agent",
			Conflicts: []api.TemplateConflict{{Path: "docker/Dockerfile", Reason: "locally-modified"}},
		})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--operator", server.URL, "service", "update", "agent", "--template", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "conflict docker/Dockerfile") || !strings.Contains(output, "dry run:") {
		t.Fatalf("output = %q, want conflict and dry-run summary", output)
	}
	if strings.Contains(output, "up to date") {
		t.Fatalf("output = %q, conflict-only preview must not say up to date", output)
	}
}

func TestInitReportsTemplateAndRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader("\n"), &stdout, &stderr)
	cmd.SetArgs([]string{"init"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "ANGEE_ROOT [.angee]:") {
		t.Fatalf("prompt = %q, want ANGEE_ROOT default prompt", got)
	}
	want := "stack template dev initialized as .angee"
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("init output = %q, want %q", got, want)
	}
}

func TestTemplateInputPromptHelpChoicesAndOrder(t *testing.T) {
	root := t.TempDir()
	writeInputTemplate(t, root, `project_name:
  type: str
  default: app
  help: Machine name of the project host; also the chained project's name.
runtime_mode:
  type: str
  default: process
  help: Run framework application services as local processes or Docker containers.
  choices:
    Local process: process
    Docker container: docker
`)
	inputs, stderr, err := resolveTemplateInputsForTest(t, root, "\nprod\ndocker\n", nil, false)
	if err != nil {
		t.Fatalf("resolve inputs: %v", err)
	}
	if _, hasDefault := inputs["project_name"]; hasDefault || inputs["runtime_mode"] != "docker" {
		t.Fatalf("inputs = %#v", inputs)
	}
	for _, want := range []string{
		"  Machine name of the project host; also the chained project's name.\n",
		"\n  Run framework application services as local processes or Docker containers.\n",
		"  choices: process | docker\n",
		"project_name [app]: ",
		"runtime_mode [process]: ",
		"\nwarning: template input runtime_mode must be one of: process, docker\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q: %q", want, stderr)
		}
	}
	if strings.Index(stderr, "project_name [app]:") > strings.Index(stderr, "runtime_mode [process]:") {
		t.Errorf("questions were not in template order: %q", stderr)
	}
	if count := strings.Count(stderr, "runtime_mode [process]: "); count != 2 {
		t.Errorf("runtime prompts = %d, want 2: %q", count, stderr)
	}
}

func TestTemplateInputPromptBoolAndSecret(t *testing.T) {
	root := t.TempDir()
	writeInputTemplate(t, root, `flag:
  type: bool
  default: false
enabled:
  type: boolean
  default: true
api_key:
  type: str
  secret: true
  default: hidden-default
`)
	inputs, stderr, err := resolveTemplateInputsForTest(t, root, "yes\nno\nentered-secret\n", nil, false)
	if err != nil {
		t.Fatalf("resolve inputs: %v", err)
	}
	if inputs["flag"] != "true" || inputs["enabled"] != "false" || inputs["api_key"] != "entered-secret" {
		t.Fatalf("unexpected answers: %#v", inputs)
	}
	for _, want := range []string{"flag [y/N]: ", "enabled [Y/n]: ", "api_key: "} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q: %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "hidden-default") || strings.Contains(stderr, "entered-secret") {
		t.Fatalf("secret leaked in prompt output: %q", stderr)
	}
}

func TestTemplateInputPromptValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		question  string
		stdin     string
		want      string
		warnings  int
		promptNum int
	}{
		{
			name: "choice retry limit", question: "runtime_mode:\n  choices: [process, docker]\n",
			stdin: "prod\nprod\nprod\ndocker\n", want: "template input runtime_mode must be one of: process, docker",
			warnings: 3, promptNum: 3,
		},
		{
			name: "integer retry limit", question: "port:\n  type: int\n", stdin: "a\nb\nc\n",
			want: "template input port must be an integer", warnings: 3, promptNum: 3,
		},
		{
			name: "required", question: "topic:\n  required: true\n", stdin: "\n\n\n",
			want: "template input topic is required; pass --input topic=value", warnings: 3, promptNum: 3,
		},
		{
			name: "EOF", question: "topic:\n  required: true\n",
			want: "template input topic requires interactive input; use --yes to accept defaults or --input topic=value", promptNum: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeInputTemplate(t, root, tc.question)
			_, stderr, err := resolveTemplateInputsForTest(t, root, tc.stdin, nil, false)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if count := strings.Count(stderr, "warning: "); count != tc.warnings {
				t.Errorf("warnings = %d, want %d: %q", count, tc.warnings, stderr)
			}
			key, _, _ := strings.Cut(tc.question, ":")
			if count := strings.Count(stderr, key+": "); count != tc.promptNum {
				t.Errorf("prompts = %d, want %d: %q", count, tc.promptNum, stderr)
			}
		})
	}
}

func TestInitValidatesProvidedTemplateInputs(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{input: "runtime_mode=prod", want: "template input runtime_mode must be one of: process, docker"},
		{input: "port=not-a-number", want: "template input port must be an integer"},
		{input: "flag=perhaps", want: "template input flag must be a boolean"},
	} {
		for _, yes := range []bool{false, true} {
			name := tc.input
			if yes {
				name += " --yes"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				writeInputTemplate(t, root, "runtime_mode:\n  choices: [process, docker]\nport:\n  type: int\nflag:\n  type: bool\n")
				var stdout, stderr bytes.Buffer
				cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
				args := []string{"--root", root, "init", "--input", tc.input}
				if yes {
					args = append(args, "--yes")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err == nil || err.Error() != tc.want {
					t.Fatalf("Execute() error = %v, want %q", err, tc.want)
				}
				if stderr.Len() != 0 {
					t.Errorf("validation should happen before prompting: %q", stderr.String())
				}
			})
		}
	}
}

type templateInputsErrorPlatform struct {
	service.API
	err error
}

func (p templateInputsErrorPlatform) Template(context.Context, string) (api.TemplateDescriptor, error) {
	return api.TemplateDescriptor{}, p.err
}

func TestTemplateInputsYesRequiresDescriptorExceptPaths(t *testing.T) {
	descriptorErr := errors.New("descriptor unavailable")
	platform := templateInputsErrorPlatform{err: descriptorErr}
	for _, template := range []string{"dev", "stacks/dev", "/local/template", "../local/template"} {
		t.Run(template, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
			inputs, err := resolveStackTemplateInputs(cmd, platform, template, map[string]string{"topic": "test"}, true)
			if template == "dev" || template == "stacks/dev" {
				if !errors.Is(err, descriptorErr) {
					t.Fatalf("error = %v, want descriptor error", err)
				}
			} else if err != nil || inputs["topic"] != "test" {
				t.Fatalf("path template inputs = %#v, error = %v", inputs, err)
			}
		})
	}
}

func TestTemplateInputsYesLeavesDefaultsToRendererWithoutPrompts(t *testing.T) {
	root := t.TempDir()
	writeInputTemplate(t, root, "project_name:\n  default: app\nruntime_mode:\n  default: process\n  choices: [process, docker]\n")
	inputs, stderr, err := resolveTemplateInputsForTest(t, root, "", map[string]string{"runtime_mode": "docker", "extra": "value"}, true)
	if err != nil {
		t.Fatalf("resolve inputs: %v", err)
	}
	if _, hasDefault := inputs["project_name"]; hasDefault || inputs["runtime_mode"] != "docker" || inputs["extra"] != "value" {
		t.Fatalf("inputs = %#v", inputs)
	}
	if stderr != "" {
		t.Fatalf("--yes printed prompts: %q", stderr)
	}
}

func TestTemplateInputsYesListsMissingRequiredFlags(t *testing.T) {
	root := t.TempDir()
	writeInputTemplate(t, root, "api_key:\n  required: true\nproject_name:\n  required: true\n")
	_, stderr, err := resolveTemplateInputsForTest(t, root, "", nil, true)
	want := "template input api_key is required; pass --input api_key=value\ntemplate input project_name is required; pass --input project_name=value"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if stderr != "" {
		t.Fatalf("--yes printed prompts: %q", stderr)
	}
}

func TestInitHelpDocumentsFormFallbacks(t *testing.T) {
	for _, args := range [][]string{{"init", "--help"}, {"stack", "init", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := NewRoot(&stdout, &stderr)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(): %v", err)
			}
			for _, want := range []string{"ANGEE_ACCESSIBLE=1", "TERM=dumb", "accept template defaults and run non-interactively (no form)"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("help missing %q: %s", want, stdout.String())
				}
			}
		})
	}
}

func TestTemplateGetReadableInputs(t *testing.T) {
	root := t.TempDir()
	templatePath := writeInputTemplate(t, root, `project_name:
  default: app
  help: Machine name of the project host; also the chained project's name.
runtime_mode:
  type: str
  default: process
  choices: [process, docker]
  help: Run framework application services as local processes or Docker containers.
api_key:
  type: str
  required: true
  secret: true
token:
  default: must-not-appear
  secret: true
features:
  choices: "{{ available_features }}"
  multiselect: true
  immutable: true
  when: "{{ enabled }}"
_angee:
  kind: stack
  name: dev
  inputs:
    operator_port:
      type: int
      generated: true
`)
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--root", root, "template", "get", "stacks/dev"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	for _, want := range []string{
		"ref\tstacks/dev", "kind\tstack", "name\tdev", "path\t" + templatePath, "inputs",
		"  project_name   str  default \"app\"",
		"      Machine name of the project host; also the chained project's name.",
		"  runtime_mode   str  default \"process\"  choices: process | docker",
		"      Run framework application services as local processes or Docker",
		"      containers.",
		"  api_key        str  required  secret",
		"  token          str  default set  secret",
		"  features       str  choices: {{ available_features }}  multiselect  immutable",
		"      when: {{ enabled }}",
		"  operator_port  int  generated  read-only",
	} {
		found := false
		for _, line := range lines {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("output missing line %q:\n%s", want, output)
		}
	}
	previous := -1
	for _, key := range []string{"project_name", "runtime_mode", "api_key", "token", "features", "operator_port"} {
		position := strings.Index(output, "  "+key+" ")
		if position <= previous {
			t.Fatalf("input %s out of descriptor order:\n%s", key, output)
		}
		previous = position
	}
	if strings.Contains(output, "must-not-appear") {
		t.Fatalf("secret default leaked:\n%s", output)
	}
}

func TestTemplateHelpWrapsAt78Columns(t *testing.T) {
	help := strings.Repeat("A useful description with unicode café. ", 8)
	for _, indent := range []int{2, 6} {
		output := wrappedTemplateHelp(help, indent)
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if !strings.HasPrefix(line, strings.Repeat(" ", indent)) || utf8.RuneCountInString(line) > 78 {
				t.Errorf("invalid wrapped help line for indent %d: %q", indent, line)
			}
		}
		if strings.Join(strings.Fields(output), " ") != strings.TrimSpace(help) {
			t.Errorf("wrapped text changed: %q", output)
		}
	}
}

func resolveTemplateInputsForTest(t *testing.T, root, stdin string, provided map[string]string, yes bool) (map[string]string, string, error) {
	t.Helper()
	// An injected reader must remain scripted even when the environment would
	// otherwise allow the full-screen form.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("ANGEE_ACCESSIBLE", "")
	platform, err := service.New(root)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	var stdout, stderr bytes.Buffer
	var inputs map[string]string
	cmd := NewRootWithIO(strings.NewReader(stdin), &stdout, &stderr)
	cmd.AddCommand(&cobra.Command{
		Use: "resolve-inputs-test",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			inputs, err = resolveStackTemplateInputs(cmd, platform, "stacks/dev", provided, yes)
			return err
		},
	})
	cmd.SetArgs([]string{"resolve-inputs-test"})
	err = cmd.Execute()
	return inputs, stderr.String(), err
}

func writeInputTemplate(t *testing.T, root, questions string) string {
	t.Helper()
	templatePath := filepath.Join(root, ".templates", "stacks", "dev")
	if err := os.MkdirAll(templatePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(questions, "_angee:") {
		questions = "_angee:\n  kind: stack\n  name: dev\n" + questions
	}
	if err := os.WriteFile(filepath.Join(templatePath, "copier.yml"), []byte(questions), 0o644); err != nil {
		t.Fatal(err)
	}
	return templatePath
}

func TestStackInitFallsBackToLocalWhenOperatorUnreachable(t *testing.T) {
	// A server that is closed immediately yields a URL whose port refuses
	// connections, standing in for an operator that is configured but down.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	operatorURL := server.URL
	server.Close()

	root := t.TempDir()
	templateRoot := writeStackTemplate(t, root)
	t.Chdir(root)
	t.Setenv("ANGEE_OPERATOR_URL", operatorURL)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"stack", "init", templateRoot, "angee-notes", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes", ".angee", "angee.yaml")); err != nil {
		t.Fatalf("Stat(angee-notes/.angee/angee.yaml) error = %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "not reachable") || !strings.Contains(got, "running init locally") {
		t.Fatalf("stderr = %q, want unreachable-operator fallback notice", got)
	}
}

func TestStackInitRoutesToReachableOperator(t *testing.T) {
	initCalled := false
	descriptorCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/templates/stacks/dev":
			descriptorCalled = true
			_ = json.NewEncoder(w).Encode(api.TemplateDescriptor{Ref: "stacks/dev"})
		case r.Method == http.MethodPost && r.URL.Path == "/stack/init":
			initCalled = true
			_ = json.NewEncoder(w).Encode(service.StackInitResult{Template: "dev", Root: "/remote/angee-notes"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// Keep the HTTP client/handler boundary without requiring a local listener.
	transport := http.DefaultTransport
	http.DefaultTransport = templateInputHandlerTransport{handler: handler}
	t.Cleanup(func() { http.DefaultTransport = transport })

	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("ANGEE_OPERATOR_URL", "http://operator.test")

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"stack", "init", "dev", "angee-notes", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !initCalled {
		t.Fatal("operator /stack/init was not called; init did not route to the reachable operator")
	}
	if !descriptorCalled {
		t.Fatal("operator template descriptor was not fetched under --yes")
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes")); !os.IsNotExist(err) {
		t.Fatalf("expected no local stack when operator is reachable; Stat err = %v", err)
	}
}

type templateInputHandlerTransport struct {
	handler http.Handler
}

func (transport templateInputHandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

func TestInitRefusesNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	writeExistingStackRoot(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "--yes"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error is nil")
	}
	want := "stack template dev already exists as .angee; use --force to overwrite or `angee stack update` to update"
	if got := err.Error(); got != want {
		t.Fatalf("init error = %q, want %q", got, want)
	}
}

func TestInitForceAllowsNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	writeStackTemplate(t, root)
	writeExistingStackRoot(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "--force", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := "stack template dev initialized as .angee"
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("init output = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "angee.yaml")); err != nil {
		t.Fatalf("Stat(angee.yaml) error = %v", err)
	}
}

func TestInitTemplateFlagInitializesNamedRoot(t *testing.T) {
	root := t.TempDir()
	templateRoot := writeStackTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"init", "--template", templateRoot, "angee-notes", "--yes", "--input", "ANGEE_ROOT=."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes", "angee.yaml")); err != nil {
		t.Fatalf("Stat(angee-notes/angee.yaml) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee-notes", ".angee", "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected nested .angee manifest err = %v", err)
	}
}

func TestOperatorCommandForwardsDaemonFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"operator", "--bind", "127.0.0.1", "--port", "19000", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Run the Angee operator") || !strings.Contains(output, "--bind") {
		t.Fatalf("operator help output did not come from daemon parser:\n%s", output)
	}
}

func TestWorkspaceAliasWithoutSubcommandShowsWorkspaceHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"ws"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Manage workspaces") || !strings.Contains(output, "angee workspace [command]") {
		t.Fatalf("workspace alias output = %q, want workspace help", output)
	}
}

func TestListAliasesShowCommandHelp(t *testing.T) {
	cases := [][]string{
		{"service", "ls", "--help"},
		{"job", "ls", "--help"},
		{"source", "ls", "--help"},
		{"workspace", "ls", "--help"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		cmd := NewRoot(&stdout, &stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) error = %v", args, err)
		}
		if got := stdout.String(); !strings.Contains(got, "Aliases:") || !strings.Contains(got, "ls") {
			t.Fatalf("help for %v = %q, want ls alias", args, got)
		}
	}
}

func TestWorkspaceCreateRequiresTemplateFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"workspace", "create", "feature-a"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error is nil")
	}
	if got := err.Error(); got != "--template is required" {
		t.Fatalf("workspace create error = %q, want --template requirement", got)
	}
}

func TestStatusUsesOperatorURLFlag(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/stack/status" {
			t.Fatalf("request = %s %s, want GET /stack/status", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.StackStatusResponse{Name: "remote", Root: "/remote"})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--operator", server.URL, "--json", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !called {
		t.Fatal("operator endpoint was not called")
	}
	if got := stdout.String(); !strings.Contains(got, `"name": "remote"`) || !strings.Contains(got, `"root": "/remote"`) {
		t.Fatalf("status output = %s", got)
	}
}

func TestStatusUsesOperatorURLEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/stack/status" {
			t.Fatalf("request = %s %s, want GET /stack/status", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.StackStatusResponse{Name: "env-remote", Root: "/env"})
	}))
	defer server.Close()
	t.Setenv("ANGEE_OPERATOR_URL", server.URL)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--json", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, `"name": "env-remote"`) {
		t.Fatalf("status output = %s", got)
	}
}

func TestStatusDiscoversParentAngeeRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	if err := os.MkdirAll(filepath.Join(base, "app", "nested"), 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	stack := &manifest.Stack{Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "parent-root"}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(filepath.Join(base, "app", "nested"))

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "parent-root") || !strings.Contains(output, "root: "+root) {
		t.Fatalf("status output = %q, want discovered parent root %s", output, root)
	}
}

func TestWorkspaceCreateUsesDotAngeeForTemplatesDirectory(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceTemplate(t, root)
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{
		"workspace",
		"create",
		"feature-a",
		"--template",
		"dev-pr",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "angee.yaml")); err != nil {
		t.Fatalf("Stat(.angee/angee.yaml) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected root angee.yaml err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".angee", "workspaces", "feature-a", "README.md")); err != nil {
		t.Fatalf("Stat(workspace README) error = %v", err)
	}
}

func TestWorkspaceStatusInfersCurrentWorkspace(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	nested := filepath.Join(root, "workspaces", workspaceName, "angee-go", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "parent-root",
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {Template: "workspaces/dev-pr"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(nested)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"workspace", "status"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "workspace\t"+workspaceName+"\tready") {
		t.Fatalf("workspace status output = %q, want inferred workspace %q", got, workspaceName)
	}
}

func TestWorkspaceSyncBaseInfersCurrentWorkspace(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	nested := filepath.Join(root, "workspaces", workspaceName, "angee-go", "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace) error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "parent-root",
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {Template: "workspaces/dev-pr"},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Chdir(nested)

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"workspace", "sync-base", "--merge"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("workspace sync-base output = %q, want empty output for workspace without git sources", got)
	}
}

func writeStackTemplate(t *testing.T, root string) string {
	t.Helper()
	templateRoot := filepath.Join(root, ".templates", "stacks", "dev")
	manifestDir := filepath.Join(templateRoot, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  default: .angee
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml) error = %v", err)
	}
	manifestYAML := `version: 1
kind: stack
name: test
`
	if err := os.WriteFile(filepath.Join(manifestDir, "angee.yaml.jinja"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml.jinja) error = %v", err)
	}
	return templateRoot
}

func writeWorkspaceTemplate(t *testing.T, root string) string {
	t.Helper()
	templateRoot := filepath.Join(root, "templates", "workspaces", "dev-pr")
	templateDir := filepath.Join(templateRoot, "template")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workspace template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.yml
_angee:
  kind: workspace
  name: dev-pr
`
	if err := os.WriteFile(filepath.Join(templateRoot, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace copier.yml) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "README.md.jinja"), []byte("workspace {{ workspace_name }}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace README template) error = %v", err)
	}
	return templateRoot
}

func writeExistingStackRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".angee"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".angee", "existing"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile(existing) error = %v", err)
	}
}

func TestInitAnswersRoundTrip(t *testing.T) {
	root := t.TempDir()
	template := writeInputTemplate(t, root, `_answers_file: .copier-answers.stack.yml
project_name:
  default: original
runtime_mode:
  default: process
  choices: [process, docker]
enabled:
  type: bool
  default: false
port:
  type: int
  default: 8000
`)
	if err := os.WriteFile(filepath.Join(template, "angee.yaml.jinja"), []byte("version: 1\nkind: stack\nname: {{ project_name }}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	run := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
		cmd.SetArgs(append([]string{"--root", root}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v; stderr: %s", args, err, &stderr)
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected prompts: %s", &stderr)
		}
	}
	run("stack", "init", "dev", first, "--yes", "--input", "project_name=round-trip", "--input", "runtime_mode=docker", "--input", "enabled=true", "--input", "port=9000")
	answers := filepath.Join(first, ".copier-answers.stack.yml")
	original, err := inputform.LoadAnswersFile(answers)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"project_name": "round-trip", "runtime_mode": "docker", "enabled": "true", "port": "9000"}
	for key, value := range want {
		if original[key] != value {
			t.Fatalf("recorded input %s = %q, want %q", key, original[key], value)
		}
	}
	run("init", second, "--template", "dev", "--answers", answers, "--yes")
	replayed, err := inputform.LoadAnswersFile(filepath.Join(second, ".copier-answers.stack.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, original) {
		t.Fatalf("replayed inputs = %#v, original = %#v", replayed, original)
	}
}

func TestInitAnswersInvalidChoice(t *testing.T) {
	for _, command := range [][]string{{"init"}, {"stack", "init", "dev"}} {
		for _, yes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/yes=%v", strings.Join(command, " "), yes), func(t *testing.T) {
				root := t.TempDir()
				writeInputTemplate(t, root, "runtime_mode:\n  choices: [process, docker]\n")
				path := filepath.Join(root, "answers.yml")
				if err := os.WriteFile(path, []byte("runtime_mode: invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
				args := append([]string{"--root", root}, command...)
				args = append(args, "--answers", path)
				if yes {
					args = append(args, "--yes")
				}
				cmd.SetArgs(args)
				err := cmd.Execute()
				if err == nil || err.Error() != "template input runtime_mode must be one of: process, docker" {
					t.Fatalf("Execute() error = %v", err)
				}
				if stderr.Len() != 0 {
					t.Fatalf("unexpected prompts: %s", &stderr)
				}
			})
		}
	}
}

func TestWorkspaceCreateInputForm(t *testing.T) {
	for _, tc := range []struct {
		name, stdin, stackDefault string
		defaultRequired           bool
		emptyStackDefault         bool
		flags                     []string
		prompts                   int
	}{
		{name: "flags satisfied", flags: []string{"--input", "topic=flag"}},
		{name: "stack satisfied", stackDefault: "stack"},
		{name: "required default", defaultRequired: true},
		{name: "empty stack default", emptyStackDefault: true, stdin: "scripted\n", prompts: 1},
		{name: "blank stack default", stackDefault: " ", stdin: "scripted\n", prompts: 1},
		{name: "scripted missing", stdin: "scripted\n", prompts: 1},
		{name: "yes satisfied", flags: []string{"--input", "topic=flag", "--yes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			template := filepath.Join(root, ".templates", "workspaces", "dev")
			if err := os.MkdirAll(template, 0o755); err != nil {
				t.Fatal(err)
			}
			config := `_angee:
  kind: workspace
  name: dev
  inputs:
    topic:
      required: true
topic:
  required: true
optional:
  default: renderer-default
`
			if tc.defaultRequired {
				config = strings.Replace(config, "topic:\n  required: true", "topic:\n  required: true\n  default: default-topic", 1)
			}
			if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(template, "README.md.jinja"), []byte("{{ topic }} {{ optional }}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.stackDefault != "" || tc.emptyStackDefault {
				stack := &manifest.Stack{Version: 1, Kind: "stack", Name: "host", WorkspaceDefaults: map[string]manifest.WorkspaceDefaults{"workspaces/dev": {Inputs: map[string]string{"topic": tc.stackDefault}}}}
				if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			cmd := NewRootWithIO(strings.NewReader(tc.stdin), &stdout, &stderr)
			cmd.SetArgs(append([]string{"--root", root, "workspace", "create", "test", "--template", "dev"}, tc.flags...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(): %v; stderr: %s", err, &stderr)
			}
			if count := strings.Count(stderr.String(), "topic: "); count != tc.prompts {
				t.Fatalf("topic prompts = %d, want %d: %s", count, tc.prompts, &stderr)
			}
			if strings.Contains(stderr.String(), "optional") {
				t.Fatalf("prompted for satisfied optional value: %s", &stderr)
			}
		})
	}
}

func TestCreateInteractiveRejectsYesAndNonTerminal(t *testing.T) {
	for _, command := range [][]string{
		{"workspace", "create", "test", "--template", "dev"},
		{"service", "create", "--template", "dev", "--workspace", "test"},
	} {
		for _, yes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/yes=%v", command[0], yes), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
				args := append([]string{"--root", t.TempDir()}, command...)
				args = append(args, "-i")
				want := "--interactive requires a terminal; use --input or --answers, or pipe answers without --interactive"
				if yes {
					args = append(args, "--yes")
					want = "--interactive cannot be combined with --yes"
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err == nil || err.Error() != want {
					t.Fatalf("Execute() error = %v, want %q", err, want)
				}
			})
		}
	}
}

func TestTemplateAnswersLayering(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	cmd := NewRootWithIO(strings.NewReader(""), &stdout, &stderr)
	cmd.Flags().StringArray("answers", nil, "")
	for i, content := range []string{"topic: first\nanswer: first\n", "topic: second\nanswer: second\n"} {
		path := filepath.Join(root, fmt.Sprintf("answers%d.yml", i))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("answers", path); err != nil {
			t.Fatal(err)
		}
	}
	result, err := loadTemplateInputValues(cmd, map[string]string{"topic": "flag"}, map[string]string{"topic": "stack", "stack_only": "kept"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"topic": "flag", "answer": "second", "stack_only": "kept"}
	origins := map[string]inputform.Origin{"topic": inputform.OriginFlag, "answer": inputform.OriginAnswers, "stack_only": inputform.OriginStack}
	if !reflect.DeepEqual(result.Values, want) || !reflect.DeepEqual(result.Origins, origins) {
		t.Fatalf("result = %#v", result)
	}
}

type workspaceInputPreflightPlatform struct {
	service.API
	descriptor api.TemplateDescriptor
	responses  []api.WorkspaceCreatePreflightResponse
	requests   []api.WorkspaceCreateRequest
	ref        string
}

func (p *workspaceInputPreflightPlatform) Template(_ context.Context, ref string) (api.TemplateDescriptor, error) {
	p.ref = ref
	return p.descriptor, nil
}

func (p *workspaceInputPreflightPlatform) WorkspaceCreatePreflight(_ context.Context, req api.WorkspaceCreateRequest) (api.WorkspaceCreatePreflightResponse, error) {
	p.requests = append(p.requests, req)
	return p.responses[len(p.requests)-1], nil
}

func TestWorkspaceCreateRechecksExplicitInputs(t *testing.T) {
	for _, final := range []api.WorkspaceCreatePreflightResponse{
		{OK: true},
		{MissingRequired: []string{"server_required"}, InvalidInputs: []api.PreflightFailure{{Field: "region", Reason: "unavailable"}}},
	} {
		t.Run(fmt.Sprintf("ok=%v", final.OK), func(t *testing.T) {
			platform := &workspaceInputPreflightPlatform{
				descriptor: api.TemplateDescriptor{Inputs: []api.TemplateInputDescriptor{
					{Name: "topic", Required: true, Question: true},
					{Name: "region", Default: "template", Question: true},
					{Name: "default_only", Default: "renderer", Question: true},
				}},
				responses: []api.WorkspaceCreatePreflightResponse{
					{StackDefaults: map[string]string{"region": "stack"}, MissingRequired: []string{"topic"}},
					final,
				},
			}
			var stdout, stderr bytes.Buffer
			cmd := NewRootWithIO(strings.NewReader("answer\n"), &stdout, &stderr)
			cmd.SetContext(context.Background())
			got, err := resolveWorkspaceTemplateInputs(cmd, platform, api.WorkspaceCreateRequest{Name: "feature", Template: "dev"}, inputform.ModeScripted, false)
			if final.OK {
				if err != nil || !reflect.DeepEqual(got, map[string]string{"topic": "answer", "region": "stack"}) {
					t.Fatalf("inputs = %#v, error = %v", got, err)
				}
			} else if err == nil || err.Error() != "template input server_required is required; pass --input server_required=value\ntemplate input region: unavailable" {
				t.Fatalf("preflight error = %v", err)
			}
			if len(platform.requests) != 2 || len(platform.requests[0].Inputs) != 0 || !reflect.DeepEqual(platform.requests[1].Inputs, map[string]string{"topic": "answer", "region": "stack"}) {
				t.Fatalf("preflight requests = %#v", platform.requests)
			}
			if platform.ref != "workspaces/dev" || strings.Count(stderr.String(), "topic: ") != 1 {
				t.Fatalf("ref = %q, prompts = %q", platform.ref, stderr.String())
			}
		})
	}
}
