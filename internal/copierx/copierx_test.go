package copierx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTemplateQuestionsPreservesFileOrderAndSkippedKeys(t *testing.T) {
	names := []string{"zulu", "alpha", "project_name", "runtime_mode", "port", "database", "cache", "worker", "ingress", "domain", "image", "addons"}
	var body strings.Builder
	body.WriteString("_angee:\n  kind: stack\n  inputs:\n    metadata_only:\n      type: str\n_private:\n  default: hidden\nscalar: ignored\nsequence: [ignored]\n")
	for _, name := range names {
		body.WriteString(name + ":\n  type: str\n  default: " + name + "-default\n")
	}
	body.WriteString("invalid:\n  required: not-a-bool\n  default: still-a-default\n")
	templatePath := writeTemplate(t, filepath.Join(t.TempDir(), "tpl"), body.String())
	questions, defaults, err := TemplateQuestions(templatePath)
	if err != nil {
		t.Fatalf("TemplateQuestions: %v", err)
	}
	if len(questions) != len(names) {
		t.Fatalf("questions = %#v, want exactly %d top-level questions", questions, len(names))
	}
	for order, name := range names {
		question, ok := questions[name]
		if !ok || question.Order != order {
			t.Errorf("question %s order = %d (present=%v), want %d", name, question.Order, ok, order)
		}
		if defaults[name] != name+"-default" {
			t.Errorf("default %s = %q, want %q", name, defaults[name], name+"-default")
		}
	}
	if len(defaults) != len(names)+1 || defaults["invalid"] != "still-a-default" {
		t.Fatalf("defaults = %#v, want question defaults and the invalid question's default only", defaults)
	}
}

func TestTemplateQuestionsParsesQuestionFieldsAndOrderedChoices(t *testing.T) {
	templatePath := writeTemplate(t, filepath.Join(t.TempDir(), "tpl"), `runtime_mode:
  type: str
  help: Choose how services run.
  placeholder: Pick a runtime.
  secret: true
  multiselect: true
  validator: "{% if not runtime_mode %}Required{% endif %}"
  when: "{{ enable_runtime }}"
  choices:
    - process
    - docker
    - 42
    - true
  default: process
flavor:
  choices:
    Zulu flavor: zulu
    Alpha flavor: alpha
    Local process: process
  when: true
dynamic:
  choices: "{{ available_flavors }}"
  when: false
`)
	questions, _, err := TemplateQuestions(templatePath)
	if err != nil {
		t.Fatalf("TemplateQuestions: %v", err)
	}
	runtimeMode := questions["runtime_mode"]
	if runtimeMode.Help != "Choose how services run." || runtimeMode.Placeholder != "Pick a runtime." || !runtimeMode.Secret || !runtimeMode.Multiselect || runtimeMode.Validator != "{% if not runtime_mode %}Required{% endif %}" || runtimeMode.When != "{{ enable_runtime }}" {
		t.Errorf("runtime_mode fields = %+v", runtimeMode)
	}
	wantSequence := []Choice{{Value: "process", Label: "process"}, {Value: "docker", Label: "docker"}, {Value: "42", Label: "42"}, {Value: "true", Label: "true"}}
	if !reflect.DeepEqual(runtimeMode.Choices, wantSequence) || runtimeMode.ChoicesExpr != "" {
		t.Errorf("sequence choices = %#v, expression = %q; want %#v and no expression", runtimeMode.Choices, runtimeMode.ChoicesExpr, wantSequence)
	}
	flavor := questions["flavor"]
	wantMapping := []Choice{{Value: "zulu", Label: "Zulu flavor"}, {Value: "alpha", Label: "Alpha flavor"}, {Value: "process", Label: "Local process"}}
	if !reflect.DeepEqual(flavor.Choices, wantMapping) || flavor.ChoicesExpr != "" || flavor.When != true {
		t.Errorf("mapping choices/when = %+v, want choices %#v and when true", flavor, wantMapping)
	}
	dynamic := questions["dynamic"]
	if dynamic.Choices != nil || dynamic.ChoicesExpr != "{{ available_flavors }}" || dynamic.When != false {
		t.Errorf("dynamic choices/when = %+v, want nil choices, raw expression, and when false", dynamic)
	}
}

func TestTemplateQuestionPrecedenceAndMetadataOnlyInputs(t *testing.T) {
	body := `_angee:
  kind: stack
  inputs:
    empty_metadata:
    runtime_mode:
      type: int
      help: Metadata help.
      default: 42
      secret: true
      immutable: true
      generated: true
      choices:
        Metadata: 42
    metadata_only:
      type: str
      help: Metadata-only help.
      placeholder: Metadata placeholder.
      secret: true
      multiselect: true
      validator: "{{ valid }}"
      when: false
      choices:
        Zulu option: zulu
        Alpha option: alpha
runtime_mode:
  type: str
  help: Question help.
  default: process
  choices: [process, docker]
`
	cfg, err := parseConfig([]byte(body))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	defs := mergeInputDefs(cfg)
	question := defs["runtime_mode"]
	// The question owns what the user sees; the metadata flags that only
	// `_angee.inputs` can express survive the merge.
	if question.Type != "str" || question.Help != "Question help." || question.Default != "process" || question.Secret || question.Order != 0 || len(question.Choices) != 2 || question.Choices[0].Value != "process" {
		t.Errorf("top-level question did not replace metadata presentation: %+v", question)
	}
	if !question.Immutable || !question.Generated {
		t.Errorf("metadata flags lost under the question: %+v", question)
	}
	metadataOnly := defs["metadata_only"]
	if defs["empty_metadata"].Order != -1 {
		t.Errorf("empty metadata input order = %d, want -1", defs["empty_metadata"].Order)
	}
	wantChoices := []Choice{{Value: "zulu", Label: "Zulu option"}, {Value: "alpha", Label: "Alpha option"}}
	if metadataOnly.Order != -1 || metadataOnly.Help != "Metadata-only help." || metadataOnly.Placeholder != "Metadata placeholder." || !metadataOnly.Secret || !metadataOnly.Multiselect || metadataOnly.Validator != "{{ valid }}" || metadataOnly.When != false || !reflect.DeepEqual(metadataOnly.Choices, wantChoices) {
		t.Errorf("metadata-only input = %+v, want enriched fields and order -1", metadataOnly)
	}
	if _, ok := cfg.Questions["metadata_only"]; ok {
		t.Fatal("metadata-only input unexpectedly became a top-level question")
	}
	if got := mergeInputs(cfg, nil)["runtime_mode"]; got != "process" {
		t.Errorf("merged runtime_mode default = %q, want process", got)
	}
}

func TestTemplateQuestionsPreservesYAMLMergeBehavior(t *testing.T) {
	body := `_base: &base
  project_path:
    type: path
    default: examples/app
  token:
    generated: true
    length: 12
  inherited:
    default: first
  runtime_mode:
    default: obsolete
_fallback: &fallback
  inherited:
    default: second
  extra:
    default: extra
first:
  default: first
<<: [*base, *fallback]
runtime_mode:
  default: process
last:
  default: last
`
	templatePath := writeTemplate(t, filepath.Join(t.TempDir(), "tpl"), body)
	questions, defaults, err := TemplateQuestions(templatePath)
	if err != nil {
		t.Fatalf("TemplateQuestions: %v", err)
	}
	wantOrder := []string{"first", "project_path", "token", "inherited", "extra", "runtime_mode", "last"}
	if len(questions) != len(wantOrder) {
		t.Fatalf("questions = %+v, want %v", questions, wantOrder)
	}
	for order, name := range wantOrder {
		if question, ok := questions[name]; !ok || question.Order != order {
			t.Errorf("question %s order = %d (present=%v), want %d", name, question.Order, ok, order)
		}
	}
	if questions["inherited"].Default != "first" || defaults["inherited"] != "first" || questions["runtime_mode"].Default != "process" || defaults["runtime_mode"] != "process" {
		t.Errorf("merge precedence differs between questions %+v and defaults %+v", questions, defaults)
	}
	inputs, err := TemplateInputs(templatePath, nil)
	if err != nil || len(inputs["token"]) != 12 {
		t.Fatalf("merged generated input = %q, error = %v; want 12 characters", inputs["token"], err)
	}
	resolved, err := ResolvePathInputs(templatePath, Inputs{"project_path": "examples/app"}, t.TempDir(), ".angee")
	if err != nil || resolved["project_path"] != "../examples/app" {
		t.Fatalf("merged path input = %q, error = %v; want ../examples/app", resolved["project_path"], err)
	}
}

func TestTemplateChoicesPreserveYAMLMergeAndAliasBehavior(t *testing.T) {
	body := `_process: &process process
_base: &base
  type: str
  help: Inherited help.
  choices: &choices
    Docker containers: &docker docker
    Local processes: *process
_angee:
  inputs:
    metadata_only:
      <<: *base
inherited:
  <<: *base
aliased: *base
mapping:
  choices:
    <<: *choices
    Docker containers: custom-docker
    Remote: remote
sequence:
  choices: [*process, *docker]
`
	cfg, err := parseConfig([]byte(body))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	wantInherited := []Choice{{Value: "docker", Label: "Docker containers"}, {Value: "process", Label: "Local processes"}}
	for _, name := range []string{"inherited", "aliased", "metadata_only"} {
		input := mergeInputDefs(cfg)[name]
		if input.Help != "Inherited help." || !reflect.DeepEqual(input.Choices, wantInherited) {
			t.Errorf("%s = %+v, want inherited help and choices %+v", name, input, wantInherited)
		}
	}
	wantMapping := []Choice{{Value: "process", Label: "Local processes"}, {Value: "custom-docker", Label: "Docker containers"}, {Value: "remote", Label: "Remote"}}
	if got := cfg.Questions["mapping"].Choices; !reflect.DeepEqual(got, wantMapping) {
		t.Errorf("merged mapping choices = %+v, want %+v", got, wantMapping)
	}
	wantSequence := []Choice{{Value: "process", Label: "process"}, {Value: "docker", Label: "docker"}}
	if got := cfg.Questions["sequence"].Choices; !reflect.DeepEqual(got, wantSequence) {
		t.Errorf("aliased sequence choices = %+v, want %+v", got, wantSequence)
	}
}

// writeTemplate stages a minimal copier template with the given
// copier.yml body and returns its absolute path.
func writeTemplate(t *testing.T, dir, copierYAML string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) = %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copier.yml"), []byte(copierYAML), 0o644); err != nil {
		t.Fatalf("write copier.yml: %v", err)
	}
	return dir
}

func TestResolvePathInputsRewritesRelativePathsAsAngeeRootRelative(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: examples/foo",
		"ANGEE_ROOT:",
		"  type: str",
		"  default: .angee",
	}, "\n"))
	dest := filepath.Join(tmp, "host")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("MkdirAll(host) = %v", err)
	}
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": "examples/foo", "ANGEE_ROOT": ".angee"}, dest, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != "../examples/foo" {
		t.Fatalf("project_path = %q, want %q", got, "../examples/foo")
	}
}

func TestResolvePathInputsWithoutAngeeRootIsIdentity(t *testing.T) {
	// A template with path inputs but no ANGEE_ROOT input renders the stack at
	// the destination itself (project-at-root, e.g. stacks/dev post-overlay):
	// relative inputs must pass through untranslated, never gain a ".." hop.
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: \".\"",
		"framework_path:",
		"  type: path",
		"  default: workspaces/src/angee-django",
	}, "\n"))
	dest := filepath.Join(tmp, "host")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("MkdirAll(host) = %v", err)
	}
	inputs := Inputs{"project_path": ".", "framework_path": "workspaces/src/angee-django"}
	out, err := ResolvePathInputs(tpl, inputs, dest, "")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != "." {
		t.Fatalf("project_path = %q, want %q", got, ".")
	}
	if got := out["framework_path"]; got != "workspaces/src/angee-django" {
		t.Fatalf("framework_path = %q, want %q", got, "workspaces/src/angee-django")
	}
}

func TestResolvePathInputsKeepsAbsolutePathsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: \"/abs/dummy\"",
	}, "\n"))
	abs := "/some/absolute/path"
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": abs, "ANGEE_ROOT": ".angee"}, tmp, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != abs {
		t.Fatalf("project_path = %q, want %q (absolute should pass through)", got, abs)
	}
}

func TestResolvePathInputsHonoursDeeperAngeeRoot(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_path:",
		"  type: path",
		"  default: \".\"",
	}, "\n"))
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": ".", "ANGEE_ROOT": "state/dev"}, tmp, "state/dev")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	// "." resolves to dest itself; relative from <dest>/state/dev is "../..".
	if got := out["project_path"]; got != "../.." {
		t.Fatalf("project_path = %q, want %q", got, "../..")
	}
}

func TestResolvePathInputsLeavesNonPathInputsAlone(t *testing.T) {
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: stack",
		"  name: dev",
		"project_name:",
		"  type: str",
		"  default: foo",
		"port:",
		"  type: int",
		"  default: 8100",
	}, "\n"))
	out, err := ResolvePathInputs(tpl, Inputs{"project_name": "foo", "port": "8100", "extra": "untouched"}, tmp, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if out["project_name"] != "foo" || out["port"] != "8100" || out["extra"] != "untouched" {
		t.Fatalf("non-path inputs were mutated: %#v", out)
	}
}

func TestResolvePathInputsHandlesAngeeInputsBlock(t *testing.T) {
	// Workspace templates conventionally declare inputs under `_angee.inputs`
	// rather than at top level. Both forms must trigger path resolution.
	tmp := t.TempDir()
	tpl := writeTemplate(t, filepath.Join(tmp, "tpl"), strings.Join([]string{
		"_angee:",
		"  kind: workspace",
		"  name: dev",
		"  inputs:",
		"    project_path:",
		"      type: path",
		"      default: examples/foo",
	}, "\n"))
	dest := filepath.Join(tmp, "host")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("MkdirAll = %v", err)
	}
	out, err := ResolvePathInputs(tpl, Inputs{"project_path": "examples/foo"}, dest, ".angee")
	if err != nil {
		t.Fatalf("ResolvePathInputs() = %v", err)
	}
	if got := out["project_path"]; got != "../examples/foo" {
		t.Fatalf("project_path = %q, want %q", got, "../examples/foo")
	}
}

// ParseMetadata is the entry point for callers that already hold copier.yml
// bytes from a guarded read, so it must agree with ReadMetadata on well-formed
// input and fail loudly rather than silently on malformed input.
func TestParseMetadata(t *testing.T) {
	t.Run("reads angee metadata", func(t *testing.T) {
		metadata, err := ParseMetadata([]byte("_subdirectory: template\n_angee:\n  kind: stack\n  name: dev\n  include_root: \"../..\"\n"))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if metadata.Kind != "stack" || metadata.Name != "dev" {
			t.Fatalf("metadata kind/name = %q/%q, want stack/dev", metadata.Kind, metadata.Name)
		}
		if metadata.IncludeRoot != "../.." {
			t.Fatalf("IncludeRoot = %q, want ../..", metadata.IncludeRoot)
		}
	})
	t.Run("absent angee block is zero", func(t *testing.T) {
		metadata, err := ParseMetadata([]byte("_subdirectory: template\n"))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if metadata.Kind != "" || metadata.IncludeRoot != "" {
			t.Fatalf("metadata = %+v, want the zero value", metadata)
		}
	})
	t.Run("empty input is zero", func(t *testing.T) {
		if _, err := ParseMetadata(nil); err != nil {
			t.Fatalf("ParseMetadata(nil): %v", err)
		}
	})
	t.Run("malformed yaml errors", func(t *testing.T) {
		if _, err := ParseMetadata([]byte("_angee:\n  kind: [unterminated\n")); err == nil {
			t.Fatal("ParseMetadata accepted malformed YAML")
		}
	})
	t.Run("agrees with ReadMetadata", func(t *testing.T) {
		body := "_subdirectory: template\n_angee:\n  kind: stack\n  include_root: \"..\"\n"
		templatePath := writeTemplate(t, filepath.Join(t.TempDir(), "tpl"), body)
		fromPath, err := ReadMetadata(templatePath)
		if err != nil {
			t.Fatalf("ReadMetadata: %v", err)
		}
		fromBytes, err := ParseMetadata([]byte(body))
		if err != nil {
			t.Fatalf("ParseMetadata: %v", err)
		}
		if fromPath.Kind != fromBytes.Kind || fromPath.IncludeRoot != fromBytes.IncludeRoot {
			t.Fatalf("ReadMetadata = %+v, ParseMetadata = %+v", fromPath, fromBytes)
		}
	})
}

func TestMergeInputDefKeepsMetadataFlagsUnderTheQuestion(t *testing.T) {
	meta := Input{Required: true, Immutable: true, Generated: true, Length: 12, Type: "str", Help: "meta help", Order: -1}
	question := Input{Type: "int", Default: 3, Help: "question help", Order: 4, Choices: []Choice{{Value: "3", Label: "3"}}}
	merged := MergeInputDef(meta, question)
	if !merged.Required || !merged.Immutable || !merged.Generated || merged.Length != 12 {
		t.Fatalf("metadata flags lost: %+v", merged)
	}
	if merged.Type != "int" || merged.Default != 3 || merged.Help != "question help" || merged.Order != 4 || len(merged.Choices) != 1 {
		t.Fatalf("question fields lost: %+v", merged)
	}
	fallback := MergeInputDef(meta, Input{Order: 2})
	if fallback.Type != "str" || fallback.Help != "meta help" || fallback.Order != 2 {
		t.Fatalf("metadata fallback lost: %+v", fallback)
	}
}
