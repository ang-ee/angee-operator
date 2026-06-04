package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"gopkg.in/yaml.v3"
)

// StackUpdateTemplateOptions configures StackUpdateFromTemplate.
type StackUpdateTemplateOptions struct {
	// DryRun computes the merge and reports the changes without writing the
	// manifest or regenerating the derived runtime files.
	DryRun bool
}

// StackUpdateTemplateResult reports what a template re-render changed.
type StackUpdateTemplateResult struct {
	Changed bool     `json:"changed"`
	Changes []string `json:"changes,omitempty"`
}

// StackUpdateFromTemplate re-renders angee.yaml from the stack's Copier
// template, structurally merges the template-origin sections back over the
// current manifest, re-runs the template's `_angee.ensure` invariants, and —
// unless DryRun — saves the manifest and regenerates the derived runtime files.
//
// The merge preserves operator-managed runtime state verbatim (`operator`,
// `workspaces`, `port_leases`) and keeps allocated `ports` values, while
// refreshing template-origin sections and keeping user-added keys.
func (p *Platform) StackUpdateFromTemplate(ctx context.Context, opts StackUpdateTemplateOptions) (StackUpdateTemplateResult, error) {
	if err := ctx.Err(); err != nil {
		return StackUpdateTemplateResult{}, err
	}
	ours, err := p.LoadStack()
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	// Snapshot the current manifest before any merge/ensure mutation so change
	// detection compares against a pristine baseline.
	before, err := manifest.Marshal(ours)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}

	// Resolve the template: prefer the recorded template.active, fall back to
	// the answers file's _src_path.
	answersFile := ".copier-answers.yml"
	ref := ""
	if ours.Template != nil {
		if ours.Template.AnswersFile != "" {
			answersFile = ours.Template.AnswersFile
		}
		ref = ours.Template.Active
	}
	// The answers file is written by copier at the render target, which is the
	// ANGEE_ROOT or its parent (e.g. <project>/.copier-answers.yml with the
	// manifest at <project>/.angee/angee.yaml). Look in both — but only accept
	// the parent's file if its recorded ANGEE_ROOT points back to this root, so
	// an unrelated parent project's answers can't be picked up.
	srcPath, answers, ok, err := p.locateStackAnswers(answersFile)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	if !ok {
		// Without the recorded answers a re-render would fall back to template
		// defaults and silently refresh sections from the wrong inputs.
		return StackUpdateTemplateResult{}, &InvalidInputError{
			Field:  "template",
			Reason: fmt.Sprintf("missing answers file %q; re-create the stack with `angee stack init --force` before `stack update --template`", answersFile),
		}
	}
	if ref == "" {
		ref = srcPath
	}
	if ref == "" {
		return StackUpdateTemplateResult{}, &InvalidInputError{
			Field:  "template",
			Reason: "stack records no template (template.active / .copier-answers.yml); re-create with `angee stack init --force`",
		}
	}
	templatePath, _, err := p.resolveTemplate(ctx, ref, "stack")
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}

	// Render the template to a scratch dir with the recorded answers; load the
	// fresh manifest as `theirs`.
	mergedInputs, err := copierx.TemplateInputs(templatePath, answers)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	scratch, err := os.MkdirTemp("", "angee-stack-update-*")
	if err != nil {
		return StackUpdateTemplateResult{}, fmt.Errorf("create render scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	resolvedInputs, err := copierx.ResolvePathInputs(templatePath, mergedInputs, scratch, mergedInputs["ANGEE_ROOT"])
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	if err := (copierx.LocalRenderer{}).Copy(ctx, copierx.CopyRequest{Template: templatePath, Dest: scratch, Inputs: resolvedInputs}); err != nil {
		return StackUpdateTemplateResult{}, fmt.Errorf("re-render stack template: %w", err)
	}
	theirsPlatform, err := New(expectedStackRoot(scratch, mergedInputs))
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	theirs, err := theirsPlatform.LoadStack()
	if err != nil {
		return StackUpdateTemplateResult{}, fmt.Errorf("load re-rendered manifest: %w", err)
	}

	// Structured merge per the proposal's provenance table, then re-run the
	// template's ensure invariants on the result.
	merged := mergeStackFromTemplate(ours, theirs)
	metadata, err := copierx.ReadMetadata(templatePath)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	if err := manifest.Ensure(merged, metadata.Ensure); err != nil {
		return StackUpdateTemplateResult{}, err
	}

	after, err := manifest.Marshal(merged)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	result := StackUpdateTemplateResult{
		Changed: !bytes.Equal(before, after),
		Changes: summarizeStackChanges(ours, merged),
	}
	if opts.DryRun {
		return result, nil
	}
	if result.Changed {
		if err := manifest.SaveFile(manifest.Path(p.root), merged); err != nil {
			return StackUpdateTemplateResult{}, err
		}
	}
	if _, err := p.StackPrepare(ctx); err != nil {
		return StackUpdateTemplateResult{}, err
	}
	return result, nil
}

// mergeStackFromTemplate merges the freshly-rendered `theirs` over the current
// `ours`. Template-origin sections are refreshed (theirs wins for its keys,
// ours-only keys preserved); `ports` keep ours' allocated values and only gain
// new template keys; runtime sections (`operator`, `workspaces`, `port_leases`)
// are preserved verbatim from ours.
func mergeStackFromTemplate(ours, theirs *manifest.Stack) *manifest.Stack {
	merged := *ours
	merged.Version = theirs.Version
	merged.Kind = theirs.Kind
	merged.Name = theirs.Name
	merged.SecretsBackend = theirs.SecretsBackend
	if theirs.Template != nil { // refresh template metadata; keep ours if the render omitted it
		merged.Template = theirs.Template
	}
	merged.Sources = overlayMap(ours.Sources, theirs.Sources)
	merged.Secrets = overlayMap(ours.Secrets, theirs.Secrets)
	merged.Volumes = overlayMap(ours.Volumes, theirs.Volumes)
	merged.Services = overlayMap(ours.Services, theirs.Services)
	merged.Jobs = overlayMap(ours.Jobs, theirs.Jobs)
	merged.Ports = mergePorts(ours.Ports, theirs.Ports)
	return &merged
}

// mergePorts refreshes each template port's fields from theirs while preserving
// ours' allocated Value (non-zero), and keeps ours-only (user-added) ports.
func mergePorts(ours, theirs map[string]manifest.Port) map[string]manifest.Port {
	if len(ours) == 0 && len(theirs) == 0 {
		return ours
	}
	out := make(map[string]manifest.Port, len(ours)+len(theirs))
	for key, port := range ours {
		out[key] = port
	}
	for key, port := range theirs {
		if existing, ok := out[key]; ok && existing.Value != 0 {
			port.Value = existing.Value // keep the allocated value
		}
		out[key] = port
	}
	return out
}

// overlayMap returns ours unioned with theirs, with theirs winning on key
// overlap. It returns ours unchanged (preserving a nil map) when both are empty.
func overlayMap[V any](ours, theirs map[string]V) map[string]V {
	if len(ours) == 0 && len(theirs) == 0 {
		return ours
	}
	out := make(map[string]V, len(ours)+len(theirs))
	for k, v := range ours {
		out[k] = v
	}
	for k, v := range theirs {
		out[k] = v
	}
	return out
}

// summarizeStackChanges reports, per template-origin section, the keys the
// re-render adds (`+`) or refreshes with a different value (`~`), comparing the
// current manifest against the merged result so it reflects what is actually
// applied (e.g. a preserved port value shows no change).
func summarizeStackChanges(ours, merged *manifest.Stack) []string {
	var changes []string
	reportKeyChanges(&changes, "sources", ours.Sources, merged.Sources)
	reportKeyChanges(&changes, "secrets", ours.Secrets, merged.Secrets)
	reportKeyChanges(&changes, "volumes", ours.Volumes, merged.Volumes)
	reportKeyChanges(&changes, "ports", ours.Ports, merged.Ports)
	reportKeyChanges(&changes, "services", ours.Services, merged.Services)
	reportKeyChanges(&changes, "jobs", ours.Jobs, merged.Jobs)
	return changes
}

func reportKeyChanges[V any](changes *[]string, section string, ours, merged map[string]V) {
	for _, key := range sortedKeys(merged) {
		prev, existed := ours[key]
		switch {
		case !existed:
			*changes = append(*changes, fmt.Sprintf("+ %s/%s", section, key))
		case !yamlEqual(prev, merged[key]):
			*changes = append(*changes, fmt.Sprintf("~ %s/%s", section, key))
		}
	}
}

// yamlEqual compares two values by their canonical YAML rendering, so structs
// with `any` fields (e.g. Service.Build) compare correctly.
func yamlEqual(a, b any) bool {
	ab, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := yaml.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// locateStackAnswers reads the Copier answers for this stack. The answers file
// sits at the render target, which is the stack root or — when ANGEE_ROOT is a
// subdir like `.angee` — its parent. The parent file is only accepted when its
// recorded ANGEE_ROOT answer names this root's basename, so an unrelated parent
// project's answers can't be mistaken for ours.
func (p *Platform) locateStackAnswers(answersFile string) (srcPath string, inputs copierx.Inputs, ok bool, err error) {
	rootPath := filepath.Join(p.root, answersFile)
	if _, statErr := os.Stat(rootPath); statErr == nil {
		srcPath, inputs, err = readTemplateAnswers(rootPath)
		return srcPath, inputs, err == nil, err
	}
	parentPath := filepath.Join(filepath.Dir(p.root), answersFile)
	if _, statErr := os.Stat(parentPath); statErr == nil {
		srcPath, inputs, err = readTemplateAnswers(parentPath)
		if err != nil {
			return "", nil, false, err
		}
		if inputs["ANGEE_ROOT"] == filepath.Base(p.root) {
			return srcPath, inputs, true, nil
		}
	}
	return "", nil, false, nil
}

// readTemplateAnswers parses a Copier answers file, returning its recorded
// template path (`_src_path`) and the non-metadata answers as template inputs.
// Answers are stringified into the string-keyed input map (matching StackInit's
// scalar-input model); non-scalar answers are not meaningfully supported.
func readTemplateAnswers(path string) (srcPath string, inputs copierx.Inputs, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", nil, err
	}
	inputs = copierx.Inputs{}
	for key, value := range raw {
		if key == "_src_path" {
			srcPath = fmt.Sprint(value)
			continue
		}
		if strings.HasPrefix(key, "_") {
			continue
		}
		inputs[key] = fmt.Sprint(value)
	}
	return srcPath, inputs, nil
}
