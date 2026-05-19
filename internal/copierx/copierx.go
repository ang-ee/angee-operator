package copierx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fyltr/angee/internal/manifest"
	copier "github.com/fyltr/copier-go"
	"gopkg.in/yaml.v3"
)

type Inputs map[string]string

// PathInputType is the copier.yml input type that opts an input into
// angee's path resolution: the user supplies a logical path (relative
// to the destination of the render, or absolute), and angee converts
// it to ANGEE_ROOT-relative before handing it to the renderer. The
// stored manifest path is therefore portable across machines.
const PathInputType = "path"

// ResolvePathInputs walks every `type: path` input declared in the
// template (either under `_angee.inputs` or as top-level copier.yml
// questions) and rewrites its value so that the rendered manifest
// stores a path that works when resolved from ANGEE_ROOT.
//
// Resolution rules:
//   - empty value → unchanged
//   - absolute path → kept absolute (manifest.ResolvePath passes it through)
//   - relative path → resolved against destDir (the render destination,
//     i.e. the target stack root for `angee init` or the workspace
//     dir for a chained workspace render), then re-expressed relative
//     to <destDir>/<ANGEE_ROOT> via filepath.Rel — yielding the natural
//     "../..." escape that ANGEE_ROOT introduces.
//
// destDir must be the dir copier renders into (the parent of ANGEE_ROOT).
// angeeRoot must be the ANGEE_ROOT value as stored in inputs (e.g. ".angee");
// it can also be an absolute path.
func ResolvePathInputs(templatePath string, inputs Inputs, destDir, angeeRoot string) (Inputs, error) {
	if len(inputs) == 0 {
		return inputs, nil
	}
	cfg, err := readConfig(templatePath)
	if err != nil {
		return nil, err
	}
	defs := mergeInputDefs(cfg)
	hasPathInput := false
	for _, def := range defs {
		if def.Type == PathInputType {
			hasPathInput = true
			break
		}
	}
	if !hasPathInput {
		return inputs, nil
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("resolve path inputs: abs(%q): %w", destDir, err)
	}
	if angeeRoot == "" {
		angeeRoot = ".angee"
	}
	absAngeeRoot := angeeRoot
	if !filepath.IsAbs(absAngeeRoot) {
		absAngeeRoot = filepath.Join(absDest, angeeRoot)
	}
	out := make(Inputs, len(inputs))
	for key, value := range inputs {
		out[key] = value
	}
	for name, def := range defs {
		if def.Type != PathInputType {
			continue
		}
		value := out[name]
		if value == "" {
			continue
		}
		if filepath.IsAbs(value) {
			continue
		}
		resolved := filepath.Join(absDest, value)
		rel, err := filepath.Rel(absAngeeRoot, resolved)
		if err != nil {
			return nil, fmt.Errorf("resolve path input %q: %w", name, err)
		}
		out[name] = rel
	}
	return out, nil
}

// mergeInputDefs returns the union of top-level copier.yml questions
// and `_angee.inputs` definitions. Top-level questions take precedence
// when the same name is declared in both (matching the precedence
// inside mergeInputs at render time).
func mergeInputDefs(cfg config) map[string]Input {
	defs := map[string]Input{}
	for name, def := range cfg.Angee.Inputs {
		defs[name] = def
	}
	for name, def := range cfg.Questions {
		defs[name] = def
	}
	return defs
}

type CopyRequest struct {
	Template string
	Dest     string
	Inputs   Inputs
}

type UpdateRequest struct {
	Template string
	Dest     string
	Inputs   Inputs
}

type Renderer interface {
	Copy(ctx context.Context, req CopyRequest) error
	Update(ctx context.Context, req UpdateRequest) error
}

type Metadata struct {
	Kind           string                          `yaml:"kind"`
	Name           string                          `yaml:"name"`
	NamePattern    string                          `yaml:"name_pattern"`
	InstanceNaming InstanceNaming                  `yaml:"instance_naming"`
	Inputs         map[string]Input                `yaml:"inputs"`
	Sources        map[string]TemplateSource       `yaml:"sources"`
	ChainRoot      string                          `yaml:"chain_root"`
	Chain          []ChainEntry                    `yaml:"chain"`
	Ensure         map[string]any                  `yaml:"ensure"`
	Persist        map[string]manifest.PersistPath `yaml:"persist"`
}

type InstanceNaming struct {
	Pattern   string `yaml:"pattern"`
	Fallback  string `yaml:"fallback"`
	MaxLength int    `yaml:"max_length"`
}

type Input struct {
	Type      string `yaml:"type"`
	Required  bool   `yaml:"required"`
	Default   any    `yaml:"default"`
	Immutable bool   `yaml:"immutable"`
	Generated bool   `yaml:"generated"`
	Length    int    `yaml:"length"`
}

type TemplateSource struct {
	Source     string `yaml:"source"`
	Kind       string `yaml:"kind"`
	Repo       string `yaml:"repo"`
	URL        string `yaml:"url"`
	Path       string `yaml:"path"`
	DefaultRef string `yaml:"default_ref"`
	CachePath  string `yaml:"cache_path"`
	Mode       string `yaml:"mode"`
	Ref        string `yaml:"ref"`
	Branch     string `yaml:"branch"`
	Subpath    string `yaml:"subpath"`
	Optional   bool   `yaml:"optional"`
}

type ChainEntry struct {
	Template string            `yaml:"template"`
	Root     string            `yaml:"root"`
	Workdir  string            `yaml:"workdir"`
	Inputs   map[string]string `yaml:"inputs"`
}

type config struct {
	Subdirectory string           `yaml:"_subdirectory"`
	Suffix       string           `yaml:"_templates_suffix"`
	AnswersFile  string           `yaml:"_answers_file"`
	Angee        Metadata         `yaml:"_angee"`
	Defaults     Inputs           `yaml:"-"`
	Questions    map[string]Input `yaml:"-"`
}

type LocalRenderer struct{}

func ReadMetadata(templatePath string) (Metadata, error) {
	cfg, err := readConfig(templatePath)
	if err != nil {
		return Metadata{}, err
	}
	return cfg.Angee, nil
}

func (LocalRenderer) Copy(ctx context.Context, req CopyRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := readConfig(req.Template)
	if err != nil {
		return err
	}
	return copier.Copy(req.Template, req.Dest, copierOptions(cfg, req.Inputs)...)
}

func (LocalRenderer) Update(ctx context.Context, req UpdateRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := readConfig(req.Template)
	if err != nil {
		return err
	}
	return copier.Update(req.Dest, copierOptions(cfg, req.Inputs)...)
}

func copierOptions(cfg config, inputs Inputs) []copier.Option {
	return []copier.Option{
		copier.WithAnswersFile(cfg.AnswersFile),
		copier.WithData(inputsAsData(inputs)),
		copier.WithDefaults(true),
		copier.WithOverwrite(true),
		copier.WithQuiet(true),
		copier.WithSkipTasks(true),
	}
}

func inputsAsData(inputs Inputs) map[string]any {
	data := make(map[string]any, len(inputs))
	for key, value := range inputs {
		data[key] = value
	}
	return data
}

func TemplateInputs(templatePath string, inputs Inputs) (Inputs, error) {
	cfg, err := readConfig(templatePath)
	if err != nil {
		return nil, err
	}
	return mergeInputs(cfg, inputs), nil
}

func TemplateQuestions(templatePath string) (map[string]Input, Inputs, error) {
	cfg, err := readConfig(templatePath)
	if err != nil {
		return nil, nil, err
	}
	return cfg.Questions, cfg.Defaults, nil
}

func readConfig(templatePath string) (config, error) {
	data, err := os.ReadFile(filepath.Join(templatePath, "copier.yml"))
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return config{}, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err == nil {
		cfg.Defaults = defaultsFromRaw(raw)
		cfg.Questions = questionsFromRaw(raw)
	}
	if cfg.Subdirectory == "" {
		cfg.Subdirectory = "."
	}
	if cfg.Suffix == "" {
		cfg.Suffix = ".jinja"
	}
	if cfg.AnswersFile == "" {
		cfg.AnswersFile = ".copier-answers.yml"
	}
	return cfg, nil
}

func questionsFromRaw(raw map[string]any) map[string]Input {
	questions := map[string]Input{}
	for key, value := range raw {
		if strings.HasPrefix(key, "_") {
			continue
		}
		body, ok := value.(map[string]any)
		if !ok {
			continue
		}
		encoded, err := yaml.Marshal(body)
		if err != nil {
			continue
		}
		var input Input
		if err := yaml.Unmarshal(encoded, &input); err != nil {
			continue
		}
		questions[key] = input
	}
	return questions
}

func mergeInputs(cfg config, inputs Inputs) Inputs {
	mergedInputs := Inputs{}
	for key, value := range cfg.Defaults {
		mergedInputs[key] = value
	}
	for key, spec := range cfg.Questions {
		if _, ok := mergedInputs[key]; ok || !spec.Generated {
			continue
		}
		length := spec.Length
		if length == 0 {
			length = 32
		}
		mergedInputs[key] = generatedInput(length)
	}
	for key, value := range inputs {
		mergedInputs[key] = value
	}
	return mergedInputs
}

func generatedInput(length int) string {
	if length < 1 {
		length = 32
	}
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) < length {
		return encoded
	}
	return encoded[:length]
}

func defaultsFromRaw(raw map[string]any) Inputs {
	defaults := Inputs{}
	for key, value := range raw {
		if strings.HasPrefix(key, "_") {
			continue
		}
		body, ok := value.(map[string]any)
		if !ok {
			continue
		}
		defaultValue, ok := body["default"]
		if !ok {
			continue
		}
		defaults[key] = fmt.Sprint(defaultValue)
	}
	return defaults
}

func ValidateMetadata(path string, wantKind string) (Metadata, error) {
	metadata, err := ReadMetadata(path)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.Kind != wantKind {
		return Metadata{}, fmt.Errorf("template kind %q does not match %q", metadata.Kind, wantKind)
	}
	return metadata, nil
}
