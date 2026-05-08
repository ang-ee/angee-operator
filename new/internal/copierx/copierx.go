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
	InstanceNaming InstanceNaming                  `yaml:"instance_naming"`
	Inputs         map[string]Input                `yaml:"inputs"`
	Sources        map[string]TemplateSource       `yaml:"sources"`
	ChainRoot      string                          `yaml:"chain_root"`
	Chain          []ChainEntry                    `yaml:"chain"`
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
