package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/substitute"
	"gopkg.in/yaml.v3"
)

// allocRefRE matches a chain input that references a port allocation, e.g.
// "${alloc.web}" (tolerating inner whitespace, matching the substitute grammar).
// Such inputs carry the workspace's authoritative ports; all others stay sourced
// from the inner stack's answers file.
var allocRefRE = regexp.MustCompile(`\$\{\s*alloc\.`)

// StackUpdateTemplateOptions configures StackUpdateFromTemplate.
type StackUpdateTemplateOptions struct {
	// DryRun computes the merge and reports the changes without writing the
	// manifest or regenerating the derived runtime files.
	DryRun bool
	// Overwrite replaces conflicting rendered files and permits deletion of
	// locally modified files that the template no longer renders.
	Overwrite bool
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
	if err := ctx.Err(); err != nil {
		return StackUpdateTemplateResult{}, err
	}
	ours, _, exists, err := readGuardedStackDocument(p.root, p.root, "angee.yaml", nil)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	if !exists {
		return StackUpdateTemplateResult{}, fmt.Errorf("stack manifest %q does not exist", manifest.Path(p.root))
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
	renderTarget, srcPath, answers, ok, err := p.locateStackAnswers(answersFile)
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

	// A workspace inner stack's allocated ports are owned by the parent stack's
	// workspace record, not the inner stack's frozen answers file — copier can
	// reset that file to template defaults, silently dropping the allocation. If
	// this stack is a managed workspace inner stack, overlay the authoritative
	// allocated ports onto the recorded answers so the re-render honours the
	// workspace's ports; other inputs (project, sources, …) still come from the
	// answers file, so a stale workspace record can't repoint them.
	renderInputs := answers
	workspaceInner := false
	portInputs, err := p.workspacePortInputs(ctx)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	if len(portInputs) > 0 {
		workspaceInner = true
		renderInputs = copierx.Inputs{}
		for key, value := range answers {
			renderInputs[key] = value
		}
		for key, value := range portInputs {
			renderInputs[key] = value
		}
	}

	templatePath, _, err := p.resolveTemplate(ctx, ref, "stack")
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}

	mergedInputs, err := copierx.TemplateInputs(templatePath, renderInputs)
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	plan, stackDocuments, err := p.buildStackRenderPlan(ctx, templatePath, renderTarget, mergedInputs, renderPlanStatePath(p.root, "stack", ""), stackPlanOptions{InputsAlreadyResolved: workspaceInner})
	if err != nil {
		return StackUpdateTemplateResult{}, err
	}
	plan.StateRoot = p.root
	plan.TargetRoot = renderTarget
	prepared, err := copierx.PrepareReconcile(ctx, plan, copierx.ReconcileOptions{
		Mode: copierx.ReconcileUpdate, DryRun: opts.DryRun, Overwrite: opts.Overwrite,
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
			merged = mergeStackFromTemplate(current, theirs, isCurrentStack && workspaceInner)
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
// `port_leases`) are preserved verbatim from ours.
//
// For ports, the authoritative source decides who wins: normally the allocated
// value lives in ours' manifest, so `ports` keep ours' values and only gain new
// template keys (authoritativePorts=false). For a workspace inner stack the
// allocation lives in the parent stack's workspace record and was rendered into
// theirs, so theirs' values win (authoritativePorts=true) — otherwise a drifted
// ours value (e.g. a template default) would clobber the workspace allocation.
func mergeStackFromTemplate(ours, theirs *manifest.Stack, authoritativePorts bool) *manifest.Stack {
	merged := *ours
	merged.Version = theirs.Version
	merged.Kind = theirs.Kind
	merged.Name = theirs.Name
	merged.SecretsBackend = theirs.SecretsBackend
	merged.Ingress = theirs.Ingress
	if theirs.Template != nil { // refresh template metadata; keep ours if the render omitted it
		merged.Template = theirs.Template
	}
	merged.Sources = overlayMap(ours.Sources, theirs.Sources)
	merged.Secrets = overlayMap(ours.Secrets, theirs.Secrets)
	merged.Volumes = overlayMap(ours.Volumes, theirs.Volumes)
	merged.Persist = overlayMap(ours.Persist, theirs.Persist)
	merged.Services = overlayMap(ours.Services, theirs.Services)
	merged.Jobs = overlayMap(ours.Jobs, theirs.Jobs)
	if authoritativePorts {
		merged.Ports = overlayMap(ours.Ports, theirs.Ports)
	} else {
		merged.Ports = mergePorts(ours.Ports, theirs.Ports)
	}
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

// workspacePortInputs detects whether p.root is the inner stack of a managed
// workspace and, if so, returns the workspace template's chain inputs that carry
// the workspace's allocated ports — each resolved against the parent stack's
// authoritative record (e.g. {"django_port": "8104"}). These overlay the inner
// stack's frozen answers so a template re-render honours the workspace's
// allocated ports even when the answers file has drifted to template defaults.
//
// A managed workspace inner stack lives at
// <parent ANGEE_ROOT>/workspaces/<name>/<chain_root>, and the parent stack
// manifest records that workspace's inputs and resolved port allocations. Only
// chain inputs that reference an ${alloc.*} allocation are returned, so non-port
// inputs (project name/path, etc.) keep flowing from the answers file and a
// stale workspace record cannot repoint them. Returns (nil, nil) for any stack
// that is not such an inner stack (the common case).
func (p *Platform) workspacePortInputs(ctx context.Context) (copierx.Inputs, error) {
	workspacePath := filepath.Dir(p.root)        // <parent root>/workspaces/<name>
	workspacesDir := filepath.Dir(workspacePath) // <parent root>/workspaces
	if filepath.Base(workspacesDir) != "workspaces" {
		return nil, nil
	}
	hostRoot := filepath.Dir(workspacesDir) // parent stack ANGEE_ROOT
	name := filepath.Base(workspacePath)
	hostPlatform, err := New(hostRoot)
	if err != nil {
		return nil, nil
	}
	hostStack, err := hostPlatform.LoadStack()
	if err != nil {
		if os.IsNotExist(err) {
			// No parent stack manifest here: not a managed workspace inner stack.
			return nil, nil
		}
		// The parent manifest exists but is unreadable/malformed. Surface it
		// rather than silently dropping the workspace's allocated ports — that
		// silent fallback is exactly the bug this path guards against.
		return nil, fmt.Errorf("load parent stack at %s: %w", hostRoot, err)
	}
	ws, ok := hostStack.Workspaces[name]
	if !ok {
		return nil, nil
	}
	// Confirm p.root is exactly this workspace's chain root, so an unrelated
	// nested stack under workspaces/ is not mistaken for the inner stack.
	if filepath.Clean(filepath.Join(workspacePath, ws.Resolved.ChainRoot)) != filepath.Clean(p.root) {
		return nil, nil
	}
	// Without recorded allocations there is nothing authoritative to inject, so
	// let the caller use the answers file unchanged.
	if ws.Template == "" || len(ws.Resolved.Allocations) == 0 {
		return nil, nil
	}
	templatePath, _, err := hostPlatform.resolveTemplate(ctx, ws.Template, "workspace")
	if err != nil {
		return nil, fmt.Errorf("resolve workspace template %q: %w", ws.Template, err)
	}
	metadata, err := copierx.ReadMetadata(templatePath)
	if err != nil {
		return nil, err
	}
	subCtx := substitute.Context{
		Inputs:        ws.Inputs,
		Name:          name,
		Alloc:         ws.Resolved.Allocations,
		WorkspacePath: workspacePath,
	}
	portInputs := copierx.Inputs{}
	for _, entry := range metadata.Chain {
		for key, value := range entry.Inputs {
			// Only allocation-bearing inputs are authoritative here; leave the
			// rest (project name/path, browser, …) to the answers file.
			if !allocRefRE.MatchString(value) {
				continue
			}
			resolved, rerr := substitute.Resolve(value, subCtx)
			if rerr != nil {
				return nil, fmt.Errorf("resolve workspace chain input %q: %w", key, rerr)
			}
			portInputs[key] = resolved
		}
	}
	if len(portInputs) == 0 {
		return nil, nil
	}
	return portInputs, nil
}

// locateStackAnswers reads the Copier answers for this stack. The answers file
// sits at the render target, which is the stack root or — when ANGEE_ROOT is a
// subdir like `.angee` — its parent. The parent file is only accepted when its
// recorded ANGEE_ROOT answer names this root's basename, so an unrelated parent
// project's answers can't be mistaken for ours.
func (p *Platform) locateStackAnswers(answersFile string) (target string, srcPath string, inputs copierx.Inputs, ok bool, err error) {
	rootPath := filepath.Join(p.root, answersFile)
	if _, statErr := os.Stat(rootPath); statErr == nil {
		srcPath, inputs, err = readTemplateAnswers(rootPath)
		return p.root, srcPath, inputs, err == nil, err
	}
	parentPath := filepath.Join(filepath.Dir(p.root), answersFile)
	if _, statErr := os.Stat(parentPath); statErr == nil {
		srcPath, inputs, err = readTemplateAnswers(parentPath)
		if err != nil {
			return "", "", nil, false, err
		}
		if inputs["ANGEE_ROOT"] == filepath.Base(p.root) {
			return filepath.Dir(p.root), srcPath, inputs, true, nil
		}
	}
	return "", "", nil, false, nil
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
