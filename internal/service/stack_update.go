package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"gopkg.in/yaml.v3"
)

// StackUpdateTemplateOptions configures StackUpdateFromTemplate.
type StackUpdateTemplateOptions struct {
	// Inputs overrides the recorded answers.
	Inputs map[string]string
	// DryRun computes the merge and reports the changes without writing the
	// manifest or regenerating the derived runtime files.
	DryRun bool
	// Overwrite replaces conflicting rendered files and permits deletion of
	// locally modified files that the template no longer renders.
	Overwrite bool
	// Skip leaves matching existing ordinary template files untouched. It does
	// not apply to stack documents, answers, or reconciliation state.
	Skip []string
}

// StackUpdateTemplateResult reports what a template re-render changed.
type StackUpdateTemplateResult struct {
	Changed   bool               `json:"changed"`
	Changes   []string           `json:"changes,omitempty"`
	Conflicts []copierx.Conflict `json:"conflicts,omitempty"`
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
	ctx, release, err := p.beginMutation(ctx, "stack")
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return StackUpdateTemplateResult{}, err
	}
	origin, err := p.resolveStackTemplateOrigin(ctx)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	templatePath, renderTarget, answers := origin.Path, origin.RenderTarget, origin.Answers
	if err := validateImmutableTemplateInputs(origin.Ref, templatePath, answers, opts.Inputs); err != nil {
		return StackUpdateTemplateResult{}, err
	}

	// The stack's own answers file holds its inputs, ports included: a stack
	// inside a workspace's directory is a stack like any other (#110). copier
	// recorded them as the last render resolved them, so an answer the caller
	// does not change is replayed as recorded, and only the caller's other
	// inputs and the defaults of unrecorded questions are resolved (#121). A
	// value passed equal to the recorded one is that answer unchanged, as the
	// -i form and the immutability check take it.
	renderInputs := copierx.Inputs{}
	recorded := copierx.Inputs{}
	for key, value := range answers {
		renderInputs[key] = value
		if override, overridden := opts.Inputs[key]; !overridden || override == value {
			recorded[key] = value
		}
	}
	for key, value := range opts.Inputs {
		renderInputs[key] = value
	}

	mergedInputs, err := copierx.TemplateInputs(templatePath, renderInputs)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	plan, stackDocuments, err := p.buildStackRenderPlan(ctx, templatePath, renderTarget, mergedInputs, recorded, renderPlanStatePath(p.root, "stack", ""))
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	plan.StateRoot = p.root
	plan.TargetRoot = renderTarget
	reconcileSkip, err := stackUpdateSkipPatterns(renderTarget, p.root, opts.Skip)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	prepared, err := copierx.PrepareReconcile(ctx, plan, copierx.ReconcileOptions{
		Mode: copierx.ReconcileUpdate, DryRun: opts.DryRun, Overwrite: opts.Overwrite, Skip: reconcileSkip,
	})
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	defer func() { _ = prepared.Close() }()

	result := StackUpdateTemplateResult{Conflicts: prepared.Result().Conflicts}
	for _, change := range prepared.Result().Changes {
		result.Changes = append(result.Changes, summarizeRenderedFileChange(renderTarget, p.root, change))
		if change.Kind != copierx.ChangeAdopt {
			result.Changed = true
		}
	}
	if len(result.Conflicts) != 0 {
		if !opts.DryRun {
			paths := make([]string, 0, len(result.Conflicts))
			for _, conflict := range result.Conflicts {
				paths = append(paths, conflict.Path)
			}
			return result, &ConflictError{Kind: "template-files", Name: strings.Join(paths, ", "), Reason: "locally modified; use --overwrite to replace"}
		}
	}

	mergedDocuments := make(map[string][]byte, len(stackDocuments))
	documentExpectations := make(map[string]renderedDocumentExpectation, len(stackDocuments))
	foundCurrentStack := false
	var preparedStack *manifest.Stack
	for _, document := range stackDocuments {
		rendered, ok := prepared.RenderedDocument(document.Path)
		if !ok {
			return StackUpdateTemplateResult{}, fmt.Errorf("stack template did not render %s", document.Path)
		}
		theirs, err := decodeStackDocument(rendered)
		if err != nil {
			return StackUpdateTemplateResult{}, fmt.Errorf("load re-rendered manifest %s: %w", document.Path, err)
		}
		destination := filepath.Join(renderTarget, filepath.FromSlash(document.Path))
		current := (*manifest.Stack)(nil)
		documentBefore := []byte(nil)
		isCurrentStack := filepath.Clean(destination) == filepath.Clean(manifest.Path(p.root))
		loaded, documentExpectation, loadErr := readStackDocumentExpectation(prepared.OpenTargetPath, document.Path)
		if loadErr != nil {
			return StackUpdateTemplateResult{}, fmt.Errorf("load current chained manifest %s: %w", document.Path, loadErr)
		}
		exists := documentExpectation.Exists
		if exists {
			current = loaded
			documentBefore, err = manifest.Marshal(loaded)
			if err != nil {
				return StackUpdateTemplateResult{}, err
			}
		}
		documentExpectations[document.Path] = documentExpectation
		if isCurrentStack {
			if !exists {
				return StackUpdateTemplateResult{}, fmt.Errorf("current stack manifest %s disappeared during template update", document.Path)
			}
			foundCurrentStack = true
		}
		merged := theirs
		if current != nil {
			merged = mergeStackFromTemplate(current, theirs)
		}
		metadata, err := copierx.ReadMetadata(document.Template)
		if err != nil {
			return StackUpdateTemplateResult{}, err
		}
		if err := manifest.Ensure(merged, metadata.Ensure); err != nil {
			return StackUpdateTemplateResult{}, err
		}
		if isCurrentStack {
			preparedStack = merged
		}
		after, err := manifest.Marshal(merged)
		if err != nil {
			return StackUpdateTemplateResult{}, err
		}
		mergedDocuments[document.Path] = after
		if !bytes.Equal(documentBefore, after) {
			result.Changed = true
			if isCurrentStack {
				result.Changes = append(result.Changes, summarizeStackChanges(current, merged)...)
			} else {
				result.Changes = append(result.Changes, "~ manifests/"+document.Path)
			}
		}
	}
	if !foundCurrentStack {
		return StackUpdateTemplateResult{}, fmt.Errorf("stack template rendered no manifest for %s", p.root)
	}
	if opts.DryRun {
		return result, nil
	}
	compiled, resolvedSecrets, err := p.compileStackArtifacts(ctx, preparedStack)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	runtimeDocuments, runtimeDeletions, runtimeModes, err := p.runtimeArtifactDocuments(renderTarget, preparedStack, compiled, resolvedSecrets)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	runtimeExpectedPaths := make(map[string][]byte, len(runtimeDocuments)+len(runtimeDeletions))
	for path, data := range runtimeDocuments {
		runtimeExpectedPaths[path] = data
	}
	for path, deleted := range runtimeDeletions {
		if deleted {
			runtimeExpectedPaths[path] = nil
		}
	}
	runtimeExpectations, err := captureRenderedDocumentExpectations(ctx, prepared.OpenTargetPath, runtimeExpectedPaths)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	verifyRuntimeEnv := func() error { return nil }
	closeRuntimeEnv := func() error { return nil }
	openBaoRel, relErr := filepath.Rel(renderTarget, filepath.Join(p.root, "run", "secrets.env"))
	if preparedStack.SecretsBackend.Type != "openbao" {
		verifyRuntimeEnv, closeRuntimeEnv, err = p.retainActiveEnvFile(preparedStack, preparedAbsolutePathOpener(prepared, renderTarget))
		if err != nil {
			return StackUpdateTemplateResult{}, err
		}
		defer func() { _ = closeRuntimeEnv() }()
		deletionPlanned := relErr == nil && runtimeDeletions[filepath.ToSlash(openBaoRel)]
		if deletionPlanned && p.activeEnvFileUsesPath(preparedStack, filepath.Join(p.root, "run", "secrets.env")) {
			return StackUpdateTemplateResult{}, fmt.Errorf("active env-file path changed to alias obsolete OpenBao runtime output during template update")
		}
	}
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), prepared.VerifyStateRootPath); err != nil {
		return StackUpdateTemplateResult{}, err
	}
	rollbackResources, closeResources, verifyResources, err := p.stageStackResources(ctx, preparedStack, preparedAbsolutePathOpener(prepared, renderTarget))
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	defer func() { _ = closeResources() }()

	rollbackFiles, err := prepared.ApplyFiles(ctx)
	if err != nil {
		return StackUpdateTemplateResult{}, joinRollbackErrors(err, rollbackResources)
	}
	rollbackDocuments, closeDocuments, verifyDocuments, err := applyRenderedDocuments(ctx, prepared.OpenTargetPath, renderTarget, mergedDocuments, nil, nil, documentExpectations, false)
	if err != nil {
		return StackUpdateTemplateResult{}, joinRollbackErrors(err, rollbackFiles, rollbackResources)
	}
	defer func() { _ = closeDocuments() }()
	rollbackRuntime, closeRuntime, verifyRuntime, err := applyRenderedDocuments(ctx, prepared.OpenTargetPath, renderTarget, runtimeDocuments, runtimeDeletions, runtimeModes, runtimeExpectations, false)
	if err != nil {
		return StackUpdateTemplateResult{}, joinRollbackErrors(err, rollbackDocuments, rollbackFiles, rollbackResources)
	}
	defer func() { _ = closeRuntime() }()
	if err := joinRollbackErrors(prepared.VerifyTargetRootPath(), verifyDocuments, verifyRuntime, verifyResources, verifyRuntimeEnv); err != nil {
		return StackUpdateTemplateResult{}, joinRollbackErrors(err, rollbackRuntime, rollbackDocuments, rollbackFiles, rollbackResources)
	}
	if err := prepared.SaveState(ctx); err != nil {
		return StackUpdateTemplateResult{}, joinRollbackErrors(err, rollbackRuntime, rollbackDocuments, rollbackFiles, rollbackResources)
	}
	return result, nil
}

func stackUpdateSkipPatterns(renderTarget, stackRoot string, patterns []string) ([]string, error) {
	prefix, err := filepath.Rel(renderTarget, stackRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve stack skip paths: %w", err)
	}
	prefix = filepath.Clean(prefix)
	if prefix == ".." || strings.HasPrefix(filepath.ToSlash(prefix), "../") {
		return nil, fmt.Errorf("stack root %q is outside template render target %q", stackRoot, renderTarget)
	}
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, &InvalidInputError{Field: "skip", Reason: "pattern is empty"}
		}
		if filepath.IsAbs(pattern) {
			return nil, &InvalidInputError{Field: "skip", Reason: fmt.Sprintf("pattern %q must be relative to the stack root", pattern)}
		}
		clean := filepath.Clean(pattern)
		if clean == ".." || strings.HasPrefix(filepath.ToSlash(clean), "../") {
			return nil, &InvalidInputError{Field: "skip", Reason: fmt.Sprintf("pattern %q escapes the stack root", pattern)}
		}
		if prefix != "." {
			clean = filepath.Join(prefix, clean)
		}
		result = append(result, filepath.ToSlash(clean))
	}
	return result, nil
}

type stackTemplateOrigin struct {
	Ref          string
	Path         string
	RenderTarget string
	Answers      copierx.Inputs
}

// resolveStackTemplateOrigin keeps introspection and re-rendering on the same
// manifest template, answers file, and render target, including parent renders.
func (p *Platform) resolveStackTemplateOrigin(ctx context.Context) (stackTemplateOrigin, error) {
	if err := ctx.Err(); err != nil {
		return stackTemplateOrigin{}, err
	}
	ours, _, exists, err := readGuardedStackDocument(p.root, p.root, "angee.yaml", nil)
	if err != nil {
		return stackTemplateOrigin{}, err
	}
	if !exists {
		return stackTemplateOrigin{}, fmt.Errorf("stack manifest %q does not exist", manifest.Path(p.root))
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
	// The answers file is written by copier at the render target, the
	// directory ANGEE_ROOT is relative to (e.g. <project>/.copier-answers.yml
	// with the manifest at <project>/deploy/.angee/angee.yaml). Only a file
	// whose recorded ANGEE_ROOT points back to this root is accepted, so an
	// unrelated project's answers above it can't be picked up.
	renderTarget, srcPath, answers, ok, err := locateStackAnswers(p.root, "", answersFile)
	if err != nil {
		return stackTemplateOrigin{}, err
	}
	if !ok {
		// Without the recorded answers a re-render would fall back to template
		// defaults and silently refresh sections from the wrong inputs.
		return stackTemplateOrigin{}, &InvalidInputError{
			Field:  "template",
			Reason: fmt.Sprintf("missing answers file %q; re-create the stack with `angee stack init --force` before `stack update --template`", answersFile),
		}
	}
	if ref == "" {
		ref = srcPath
	}
	if ref == "" {
		return stackTemplateOrigin{}, &InvalidInputError{
			Field:  "template",
			Reason: "stack records no template (template.active / .copier-answers.yml); re-create with `angee stack init --force`",
		}
	}

	templatePath, _, err := p.resolveTemplate(ctx, ref, "stack")
	if err != nil {
		return stackTemplateOrigin{}, err
	}
	return stackTemplateOrigin{Ref: ref, Path: templatePath, RenderTarget: renderTarget, Answers: answers}, nil
}

func decodeStackDocument(data []byte) (*manifest.Stack, error) {
	var stack manifest.Stack
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&stack); err != nil {
		return nil, err
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		return nil, err
	}
	return &stack, nil
}

func summarizeRenderedFileChange(renderTarget, stackRoot string, change copierx.Change) string {
	destination := filepath.Join(renderTarget, filepath.FromSlash(change.Path))
	display := change.Path
	if rel, err := filepath.Rel(stackRoot, destination); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		display = filepath.ToSlash(rel)
	}
	prefix := map[copierx.ChangeKind]string{
		copierx.ChangeAdd:    "+",
		copierx.ChangeModify: "~",
		copierx.ChangeDelete: "-",
		copierx.ChangeAdopt:  "=",
	}[change.Kind]
	return fmt.Sprintf("%s files/%s", prefix, display)
}

// mergeStackFromTemplate merges the freshly-rendered `theirs` over the current
// `ours`. Template-origin sections are refreshed (theirs wins for its keys,
// ours-only keys preserved); runtime sections (`operator`, `workspaces`,
// `port_leases`) are preserved verbatim from ours. A port's allocated value
// lives in ours' manifest, so `ports` keep ours' values and only gain new
// template keys.
func mergeStackFromTemplate(ours, theirs *manifest.Stack) *manifest.Stack {
	merged := *ours
	merged.Version = theirs.Version
	merged.Kind = theirs.Kind
	merged.Name = theirs.Name
	merged.SecretsBackend = theirs.SecretsBackend
	merged.Ingress = theirs.Ingress
	if theirs.Template != nil { // refresh template metadata; keep ours if the render omitted it
		merged.Template = theirs.Template
	}
	// Sources: one declared by hand since the last render (absent from
	// theirs) is kept as is, and one the template renders takes the
	// template's definition, replacing local edits to it.
	merged.Sources = overlayMap(ours.Sources, theirs.Sources)
	// workspace_defaults is template-origin (the dev stack renders it from its
	// work_state_source answer); workspaces themselves stay ours — they are
	// operator-written records of what was cut.
	merged.WorkspaceDefaults = overlayMap(ours.WorkspaceDefaults, theirs.WorkspaceDefaults)
	merged.Secrets = overlayMap(ours.Secrets, theirs.Secrets)
	merged.Volumes = overlayMap(ours.Volumes, theirs.Volumes)
	merged.Persist = overlayMap(ours.Persist, theirs.Persist)
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
	reportKeyChanges(&changes, "workspace_defaults", ours.WorkspaceDefaults, merged.WorkspaceDefaults)
	reportKeyChanges(&changes, "secrets", ours.Secrets, merged.Secrets)
	reportKeyChanges(&changes, "volumes", ours.Volumes, merged.Volumes)
	reportKeyChanges(&changes, "persist", ours.Persist, merged.Persist)
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

// locateStackAnswers reads the Copier answers of the stack at root and returns
// the render target holding them. copier writes the answers file in the
// directory a relative ANGEE_ROOT is relative to: the stack root for `.`, its
// parent for `.angee`, two levels up for `deploy/.angee`. So it walks up from
// root, nearest first, and takes the first answers file that rendersStackRoot
// says put its stack at root. Files above that one are never read.
//
// Any other answers file on the way is skipped, so an unrelated project's
// answers can't be mistaken for ours. Only its ANGEE_ROOT is read, so its
// other answers may hold anything. A file on the way that can't be read as a
// YAML mapping stops the walk with an *InvalidInputError naming it: it may be
// this stack's own answers, damaged, and the walk can't tell.
//
// The walk ends at top when top is not empty, else at the filesystem root.
func locateStackAnswers(root, top, answersFile string) (target string, srcPath string, inputs copierx.Inputs, ok bool, err error) {
	for dir := root; ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, answersFile)
		data, readErr := os.ReadFile(path)
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return "", "", nil, false, unreadableAnswersError(path, readErr)
		}
		if readErr == nil {
			angeeRoot, isPath, err := recordedAngeeRoot(data)
			if err != nil {
				return "", "", nil, false, unreadableAnswersError(path, err)
			}
			if isPath && rendersStackRoot(dir, angeeRoot, root) {
				srcPath, inputs, err := parseTemplateAnswers(path, data)
				if err != nil {
					return "", "", nil, false, &InvalidInputError{Field: "template", Reason: err.Error()}
				}
				return dir, srcPath, inputs, true, nil
			}
		}
		if dir == top || filepath.Dir(dir) == dir {
			return "", "", nil, false, nil
		}
	}
}

func unreadableAnswersError(path string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err // the reason names the path once
	}
	return &InvalidInputError{
		Field:  "template",
		Reason: fmt.Sprintf("cannot read answers file %s: %v; fix or move it, since the update cannot tell whether it holds this stack's answers", path, err),
	}
}

// rendersStackRoot reports whether a render into dir with the given recorded
// ANGEE_ROOT put its stack at root, by the rule stack init places a stack with
// (expectedStackRoot): an empty answer means dir itself, an absolute one names
// root exactly, and a relative one is joined to dir, so `./deploy/.angee` and
// `deploy/.angee/` count. A relative answer that climbs out of dir with `..`,
// which stack init refuses, never matches. Paths compare cleaned, without
// resolving symlinks.
func rendersStackRoot(dir, angeeRoot, root string) bool {
	if angeeRoot != "" && !filepath.IsAbs(angeeRoot) && !filepath.IsLocal(angeeRoot) {
		return false
	}
	return expectedStackRoot(dir, map[string]string{"ANGEE_ROOT": angeeRoot}) == root
}

// recordedAngeeRoot reads only the ANGEE_ROOT answer of an answers file, as
// parseTemplateAnswers would read it, leaving the file's other answers alone.
// isPath is false when the answer is not a scalar, so it names no directory.
// A file that records no ANGEE_ROOT answers "".
func recordedAngeeRoot(data []byte) (angeeRoot string, isPath bool, err error) {
	root, err := answersMapping(data)
	if err != nil {
		return "", false, err
	}
	if root == nil {
		return "", true, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "ANGEE_ROOT" {
			continue
		}
		value, err := recordedAnswerValue(root.Content[i+1])
		text, isScalar := value.(string)
		return text, err == nil && isScalar, nil
	}
	return "", true, nil
}

var errAnswersNotMapping = errors.New("expected a YAML mapping")

// answersMapping parses an answers file into its top-level mapping, which is
// nil for an empty file.
func answersMapping(data []byte) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, nil
	}
	if root := document.Content[0]; root.Kind == yaml.MappingNode {
		return root, nil
	}
	return nil, errAnswersNotMapping
}

// readTemplateAnswers parses a Copier answers file, returning its recorded
// template path and non-metadata answers. Scalars retain their source text;
// lists use the JSON array encoding accepted by template input forms.
func readTemplateAnswers(path string) (srcPath string, inputs copierx.Inputs, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	return parseTemplateAnswers(path, data)
}

// parseTemplateAnswers is readTemplateAnswers over the file's content.
func parseTemplateAnswers(path string, data []byte) (srcPath string, inputs copierx.Inputs, err error) {
	root, err := answersMapping(data)
	if errors.Is(err, errAnswersNotMapping) {
		return "", nil, fmt.Errorf("answers file %s: %w", path, err)
	}
	if err != nil {
		return "", nil, err
	}
	inputs = copierx.Inputs{}
	if root == nil {
		return "", inputs, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if key != "_src_path" && (strings.HasPrefix(key, "_") || root.Content[i].Tag == "!!merge") {
			continue
		}
		value, err := recordedAnswerValue(root.Content[i+1])
		if err != nil {
			return "", nil, fmt.Errorf("answers file %s: key %s: %w", path, key, err)
		}
		var text string
		if scalar, ok := value.(string); ok {
			text = scalar
		} else {
			encoded, _ := json.Marshal(value) // recordedAnswerValue returns only strings and lists.
			text = string(encoded)
		}
		if key == "_src_path" {
			srcPath = text
		} else {
			inputs[key] = text
		}
	}
	return srcPath, inputs, nil
}

func recordedAnswerValue(node *yaml.Node) (any, error) {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return "", nil
		}
		return node.Value, nil
	case yaml.SequenceNode:
		items := make([]any, len(node.Content))
		for i, item := range node.Content {
			var err error
			items[i], err = recordedAnswerValue(item)
			if err != nil {
				return nil, err
			}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("only scalars and lists are supported")
	}
}
