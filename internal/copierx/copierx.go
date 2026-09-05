package copierx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/internal/manifest"
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
		// A template without an ANGEE_ROOT input renders the stack at the
		// destination itself (project-at-root) — translation is identity.
		angeeRoot = "."
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

type Metadata struct {
	Kind           string                          `yaml:"kind"`
	Name           string                          `yaml:"name"`
	NamePattern    string                          `yaml:"name_pattern"`
	InstanceNaming InstanceNaming                  `yaml:"instance_naming"`
	Inputs         map[string]Input                `yaml:"inputs"`
	Sources        map[string]TemplateSource       `yaml:"sources"`
	IncludeRoot    string                          `yaml:"include_root"`
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
	Type        string   `yaml:"type"`
	Required    bool     `yaml:"required"`
	Default     any      `yaml:"default"`
	Immutable   bool     `yaml:"immutable"`
	Generated   bool     `yaml:"generated"`
	Length      int      `yaml:"length"`
	Help        string   `yaml:"help"`
	Placeholder string   `yaml:"placeholder"`
	Secret      bool     `yaml:"secret"`
	Multiselect bool     `yaml:"multiselect"`
	Validator   string   `yaml:"validator"`
	When        any      `yaml:"when"`
	Order       int      `yaml:"-"`
	Choices     []Choice `yaml:"-"`
	ChoicesExpr string   `yaml:"-"`
}

// Choice preserves the displayed label and the value passed to Copier.
type Choice struct {
	Value string
	Label string
}

// UnmarshalYAML retains choice order for both Copier questions and _angee.inputs.
func (input *Input) UnmarshalYAML(node *yaml.Node) error {
	type fields Input
	var decoded fields
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*input = Input(decoded)
	for _, entry := range orderedYAMLMapping(node) {
		if entry.key.Value != "choices" {
			continue
		}
		choices := dereferenceYAMLNode(entry.value)
		// Copier's extended form, where a choice value is itself a mapping
		// with value/validator keys, is not modelled: such entries are
		// skipped, so a template using only that form degrades to free text.
		switch choices.Kind {
		case yaml.SequenceNode:
			for _, choice := range choices.Content {
				choice = dereferenceYAMLNode(choice)
				if choice.Kind == yaml.ScalarNode {
					input.Choices = append(input.Choices, Choice{Value: choice.Value, Label: choice.Value})
				}
			}
		case yaml.MappingNode:
			for _, choice := range orderedYAMLMapping(choices) {
				label, value := dereferenceYAMLNode(choice.key), dereferenceYAMLNode(choice.value)
				if label.Kind == yaml.ScalarNode && value.Kind == yaml.ScalarNode {
					input.Choices = append(input.Choices, Choice{Value: value.Value, Label: label.Value})
				}
			}
		case yaml.ScalarNode:
			if choices.Tag == "!!str" {
				input.ChoicesExpr = choices.Value
			}
		}
	}
	return nil
}

type yamlMappingEntry struct {
	key, value *yaml.Node
}

// orderedYAMLMapping expands merges where they are declared. Explicit keys win,
// and earlier mappings in a merge sequence take precedence, as in YAML decoding.
func orderedYAMLMapping(node *yaml.Node) []yamlMappingEntry {
	visiting := map[*yaml.Node]bool{}
	var expand func(*yaml.Node) []yamlMappingEntry
	expand = func(node *yaml.Node) []yamlMappingEntry {
		node = dereferenceYAMLNode(node)
		if node.Kind != yaml.MappingNode || visiting[node] {
			return nil
		}
		visiting[node] = true
		defer delete(visiting, node)
		explicit := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := dereferenceYAMLNode(node.Content[i])
			if key.Tag != "!!merge" {
				explicit[key.Value] = true
			}
		}
		var entries []yamlMappingEntry
		merged := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := dereferenceYAMLNode(node.Content[i]), node.Content[i+1]
			if key.Tag != "!!merge" {
				entries = append(entries, yamlMappingEntry{key: key, value: value})
				continue
			}
			value = dereferenceYAMLNode(value)
			mappings := []*yaml.Node{value}
			if value.Kind == yaml.SequenceNode {
				mappings = value.Content
			}
			for _, mapping := range mappings {
				for _, entry := range expand(mapping) {
					if !explicit[entry.key.Value] && !merged[entry.key.Value] {
						entries = append(entries, entry)
						merged[entry.key.Value] = true
					}
				}
			}
		}
		return entries
	}
	return expand(node)
}

func dereferenceYAMLNode(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return node.Alias
	}
	return node
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

// config models the copier.yml keys copierx itself consumes. `_preserve_symlinks`
// is deliberately absent: the renderer (copier-go) reads it straight off the
// template config and emits symlinks verbatim, and safe reconciliation already
// treats a symlink as a first-class entry kind (fingerprint + apply), so copierx
// needs no view of the flag.
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

// ParseMetadata reads `_angee` out of copier.yml bytes a caller already holds.
// Callers that obtained the bytes through a guarded read use this instead of
// ReadMetadata so the config is not re-opened by pathname, which would let the
// file change between the two reads.
func ParseMetadata(data []byte) (Metadata, error) {
	cfg, err := parseConfig(data)
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
	return parseConfig(data)
}

func parseConfig(data []byte) (config, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return config{}, err
	}
	var cfg config
	if err := document.Decode(&cfg); err != nil {
		return config{}, err
	}
	for name, input := range cfg.Angee.Inputs {
		input.Order = -1
		cfg.Angee.Inputs[name] = input
	}
	var raw map[string]any
	if err := document.Decode(&raw); err == nil {
		cfg.Defaults = defaultsFromRaw(raw)
		cfg.Questions = questionsFromRaw(&document)
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

func questionsFromRaw(raw *yaml.Node) map[string]Input {
	questions := map[string]Input{}
	if raw.Kind == yaml.DocumentNode && len(raw.Content) > 0 {
		raw = raw.Content[0]
	}
	if raw.Kind != yaml.MappingNode {
		return questions
	}
	order := 0
	for _, entry := range orderedYAMLMapping(raw) {
		key, value := entry.key.Value, dereferenceYAMLNode(entry.value)
		if strings.HasPrefix(key, "_") {
			continue
		}
		if value.Kind != yaml.MappingNode {
			continue
		}
		var input Input
		if err := value.Decode(&input); err != nil {
			continue
		}
		input.Order = order
		order++
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
