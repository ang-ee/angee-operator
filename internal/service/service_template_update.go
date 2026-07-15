package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"gopkg.in/yaml.v3"
)

func (p *Platform) ServiceUpdateFromTemplate(ctx context.Context, name string, req api.ServiceUpdateTemplateRequest) (api.ServiceTemplateUpdateResult, error) {
	if name == "" {
		return api.ServiceTemplateUpdateResult{}, &InvalidInputError{Field: "name", Reason: "service name is required"}
	}
	var result api.ServiceTemplateUpdateResult
	lock := fslock.RootLock(p.root)
	if err := lock.With(ctx, func() error {
		updated, err := p.serviceUpdateFromTemplateLocked(ctx, name, req)
		if err != nil {
			return err
		}
		result = updated
		return nil
	}); err != nil {
		return result, err
	}
	if !req.DryRun {
		if _, err := p.StackPrepare(ctx); err != nil {
			return api.ServiceTemplateUpdateResult{}, fmt.Errorf("re-render compose after service template update: %w", err)
		}
	}
	return result, nil
}

func (p *Platform) serviceUpdateFromTemplateLocked(ctx context.Context, name string, req api.ServiceUpdateTemplateRequest) (api.ServiceTemplateUpdateResult, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	current, ok := stack.Services[name]
	if !ok {
		return api.ServiceTemplateUpdateResult{}, &NotFoundError{Kind: "service", Name: name}
	}
	stackBefore, err := manifest.Marshal(stack)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	buildContext := filepath.Join(p.root, "services", name)
	statePath := renderPlanStatePath(p.root, "services", name)
	state, hasState, err := copierx.ReadRenderState(statePath)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	templateRef, answersPath, err := serviceTemplateOrigin(buildContext, state, hasState)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	sourceFromAnswers, answers, err := readTemplateAnswers(answersPath)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, fmt.Errorf("read service template answers %q: %w", answersPath, err)
	}
	if templateRef == "" {
		templateRef = sourceFromAnswers
	}
	if templateRef == "" {
		return api.ServiceTemplateUpdateResult{}, &InvalidInputError{Field: "template", Reason: "service template origin is missing"}
	}
	workspaceName := answers["workspace_name"]
	if workspaceName == "" {
		return api.ServiceTemplateUpdateResult{}, &InvalidInputError{Field: "template", Reason: "service answers do not record workspace_name; recreate the service before template update"}
	}
	if _, ok := stack.Workspaces[workspaceName]; !ok {
		return api.ServiceTemplateUpdateResult{}, &NotFoundError{Kind: "workspace", Name: workspaceName}
	}
	for key := range req.Inputs {
		if isReservedServiceTemplateInput(key) {
			return api.ServiceTemplateUpdateResult{}, &InvalidInputError{Field: "input", Reason: fmt.Sprintf("%q is managed by the service and cannot be overridden", key)}
		}
	}
	templatePath, _, err := p.resolveTemplate(ctx, templateRef, "service")
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	metadata, err := copierx.ValidateMetadata(templatePath, "service")
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	if err := manifest.Ensure(stack, metadata.Ensure); err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	inputs := mergeServiceInputs(metadata, answers)
	for key, value := range req.Inputs {
		inputs[key] = value
	}
	allocations, err := p.allocateServicePorts(stack, name)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	workspacePath := filepath.Join(p.root, "workspaces", workspaceName)
	renderInputs := copierx.Inputs{}
	for key, value := range inputs {
		if !isReservedServiceTemplateInput(key) {
			renderInputs[key] = value
		}
	}
	renderInputs["service_name"] = name
	renderInputs["workspace_name"] = workspaceName
	renderInputs["workspace_path"] = workspacePath
	for pool, port := range allocations {
		renderInputs["alloc_"+pool] = strconv.Itoa(port)
	}
	prepared, err := copierx.PrepareReconcile(ctx, copierx.RenderPlan{
		Target: buildContext, StatePath: statePath,
		Layers:    []copierx.RenderLayer{{Name: "service", Template: templatePath, Inputs: renderInputs}},
		Documents: []string{"service.yaml"},
	}, copierx.ReconcileOptions{Mode: copierx.ReconcileUpdate, DryRun: req.DryRun, Overwrite: req.Overwrite})
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	defer prepared.Close()
	rendered, ok := prepared.RenderedDocument("service.yaml")
	if !ok {
		return api.ServiceTemplateUpdateResult{}, fmt.Errorf("service template rendered no service.yaml")
	}
	base := state.Documents["service.yaml"]
	merged, serviceChanges, serviceConflicts, err := mergeRenderedService(base, current, rendered, name, req.Overwrite)
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	fileResult := prepared.Result()
	result := api.ServiceTemplateUpdateResult{Name: name}
	for _, change := range append(fileResult.Changes, serviceChanges...) {
		result.Changes = append(result.Changes, api.TemplateChange{Path: change.Path, Kind: string(change.Kind)})
		if change.Kind != copierx.ChangeAdopt {
			result.Changed = true
		}
	}
	for _, conflict := range append(fileResult.Conflicts, serviceConflicts...) {
		result.Conflicts = append(result.Conflicts, api.TemplateConflict{Path: conflict.Path, Reason: string(conflict.Reason)})
	}
	if len(result.Conflicts) != 0 {
		paths := make([]string, 0, len(result.Conflicts))
		for _, conflict := range result.Conflicts {
			paths = append(paths, conflict.Path)
		}
		return result, &ConflictError{Kind: "service-template", Name: name, Reason: fmt.Sprintf("conflicting paths: %s; use --overwrite to replace", strings.Join(paths, ", "))}
	}
	if req.DryRun {
		return result, nil
	}
	rollbackFiles, err := prepared.ApplyFiles()
	if err != nil {
		return api.ServiceTemplateUpdateResult{}, err
	}
	if isRouted(stack, merged) {
		releaseServicePortLeases(stack, name)
	}
	ensureServiceSecrets(stack, merged)
	stack.Services[name] = merged
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		_ = rollbackFiles()
		return api.ServiceTemplateUpdateResult{}, err
	}
	if err := prepared.SaveState(); err != nil {
		_ = writeRenderedDocument(manifest.Path(p.root), stackBefore)
		_ = rollbackFiles()
		return api.ServiceTemplateUpdateResult{}, err
	}
	return result, nil
}

func serviceTemplateOrigin(buildContext string, state copierx.RenderState, hasState bool) (templateRef, answersPath string, err error) {
	if hasState {
		if len(state.Layers) != 1 {
			return "", "", &InvalidInputError{Field: "template", Reason: fmt.Sprintf("service render state must contain exactly one layer, got %d", len(state.Layers))}
		}
		layer := state.Layers[0]
		if layer.AnswersFile == "" {
			return "", "", &InvalidInputError{Field: "template", Reason: "service render state records no answers file"}
		}
		answersPath = filepath.Clean(filepath.Join(buildContext, filepath.FromSlash(layer.AnswersFile)))
		if rel, relErr := filepath.Rel(buildContext, answersPath); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", &InvalidInputError{Field: "template", Reason: "service answers path escapes build context"}
		}
		return layer.Template, answersPath, nil
	}
	answersPath = filepath.Join(buildContext, ".copier-answers.yml")
	if _, err := os.Stat(answersPath); err != nil {
		if os.IsNotExist(err) {
			return "", "", &InvalidInputError{Field: "template", Reason: "service has no render state or top-level .copier-answers.yml; recreate it before template update"}
		}
		return "", "", err
	}
	return "", answersPath, nil
}

func isReservedServiceTemplateInput(key string) bool {
	return key == "service_name" || key == "workspace_name" || key == "workspace_path" || strings.HasPrefix(key, "alloc_")
}

func mergeRenderedService(base []byte, current manifest.Service, rendered []byte, name string, overwrite bool) (manifest.Service, []copierx.Change, []copierx.Conflict, error) {
	newService, err := serviceFromRenderedDocument(rendered, name)
	if err != nil {
		return manifest.Service{}, nil, nil, err
	}
	currentValue, err := serviceAsMap(current)
	if err != nil {
		return manifest.Service{}, nil, nil, err
	}
	newValue, err := serviceAsMap(newService)
	if err != nil {
		return manifest.Service{}, nil, nil, err
	}
	root := "services." + name
	if len(base) == 0 {
		if reflect.DeepEqual(currentValue, newValue) {
			return current, []copierx.Change{{Path: root, Kind: copierx.ChangeAdopt}}, nil, nil
		}
		if !overwrite {
			return current, nil, []copierx.Conflict{{Path: root, Reason: copierx.ConflictUntrackedDifferent}}, nil
		}
		if err := validateRenderedServiceBuildContext(newService, name); err != nil {
			return manifest.Service{}, nil, nil, err
		}
		if err := validateService(name, newService); err != nil {
			return manifest.Service{}, nil, nil, err
		}
		return newService, []copierx.Change{{Path: root, Kind: copierx.ChangeModify}}, nil, nil
	}
	baseService, err := serviceFromRenderedDocument(base, name)
	if err != nil {
		return manifest.Service{}, nil, nil, fmt.Errorf("parse previous rendered service: %w", err)
	}
	baseValue, err := serviceAsMap(baseService)
	if err != nil {
		return manifest.Service{}, nil, nil, err
	}
	mergedValue, exists, changes, conflicts := mergeServiceValue(root, baseValue, true, currentValue, true, newValue, true, overwrite)
	if !exists {
		return manifest.Service{}, changes, conflicts, fmt.Errorf("service merge removed %s", root)
	}
	encoded, err := yaml.Marshal(mergedValue)
	if err != nil {
		return manifest.Service{}, nil, nil, err
	}
	var merged manifest.Service
	if err := yaml.Unmarshal(encoded, &merged); err != nil {
		return manifest.Service{}, nil, nil, err
	}
	if len(conflicts) == 0 {
		if err := validateRenderedServiceBuildContext(merged, name); err != nil {
			return manifest.Service{}, nil, nil, err
		}
		if err := validateService(name, merged); err != nil {
			return manifest.Service{}, nil, nil, err
		}
	}
	return merged, changes, conflicts, nil
}

func serviceFromRenderedDocument(data []byte, name string) (manifest.Service, error) {
	parsed, err := parsePartialServiceManifest(data)
	if err != nil {
		return manifest.Service{}, err
	}
	if len(parsed.Services) != 1 {
		return manifest.Service{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service.yaml must declare exactly one service, got %d", len(parsed.Services))}
	}
	service, renderedName := singleService(parsed.Services)
	if renderedName != name {
		return manifest.Service{}, &InvalidInputError{Field: "template", Reason: fmt.Sprintf("rendered service key %q does not match existing name %q", renderedName, name)}
	}
	return service, nil
}

func serviceAsMap(service manifest.Service) (map[string]any, error) {
	data, err := yaml.Marshal(service)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func mergeServiceValue(path string, base any, baseExists bool, current any, currentExists bool, next any, nextExists bool, overwrite bool) (any, bool, []copierx.Change, []copierx.Conflict) {
	baseMap, baseIsMap := base.(map[string]any)
	currentMap, currentIsMap := current.(map[string]any)
	nextMap, nextIsMap := next.(map[string]any)
	if currentExists && nextExists && currentIsMap && nextIsMap && (!baseExists || baseIsMap) {
		keys := map[string]struct{}{}
		if baseIsMap {
			for key := range baseMap {
				keys[key] = struct{}{}
			}
		}
		for key := range currentMap {
			keys[key] = struct{}{}
		}
		for key := range nextMap {
			keys[key] = struct{}{}
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		merged := map[string]any{}
		var changes []copierx.Change
		var conflicts []copierx.Conflict
		for _, key := range ordered {
			baseChild, baseChildExists := baseMap[key]
			currentChild, currentChildExists := currentMap[key]
			nextChild, nextChildExists := nextMap[key]
			value, exists, childChanges, childConflicts := mergeServiceValue(path+"."+key, baseChild, baseIsMap && baseChildExists, currentChild, currentChildExists, nextChild, nextChildExists, overwrite)
			if exists {
				merged[key] = value
			}
			changes = append(changes, childChanges...)
			conflicts = append(conflicts, childConflicts...)
		}
		return merged, true, changes, conflicts
	}

	selectValue := func(value any, exists bool) (any, bool, []copierx.Change, []copierx.Conflict) {
		change := serviceValueChange(path, current, currentExists, value, exists)
		return value, exists, change, nil
	}
	if !baseExists {
		switch {
		case !currentExists:
			return selectValue(next, nextExists)
		case !nextExists || reflect.DeepEqual(current, next):
			return current, true, nil, nil
		case overwrite:
			return selectValue(next, true)
		default:
			return current, true, nil, []copierx.Conflict{{Path: path, Reason: copierx.ConflictLocallyModified}}
		}
	}
	if !currentExists && !nextExists {
		return nil, false, nil, nil
	}
	if !currentExists {
		if reflect.DeepEqual(next, base) {
			return nil, false, nil, nil
		}
		if overwrite {
			return selectValue(next, nextExists)
		}
		return nil, false, nil, []copierx.Conflict{{Path: path, Reason: copierx.ConflictLocallyModified}}
	}
	if !nextExists {
		if reflect.DeepEqual(current, base) || overwrite {
			return selectValue(nil, false)
		}
		return current, true, nil, []copierx.Conflict{{Path: path, Reason: copierx.ConflictLocallyModified}}
	}
	switch {
	case reflect.DeepEqual(current, base):
		return selectValue(next, true)
	case reflect.DeepEqual(next, base), reflect.DeepEqual(current, next):
		return current, true, nil, nil
	case overwrite:
		return selectValue(next, true)
	default:
		return current, true, nil, []copierx.Conflict{{Path: path, Reason: copierx.ConflictLocallyModified}}
	}
}

func serviceValueChange(path string, current any, currentExists bool, merged any, mergedExists bool) []copierx.Change {
	if currentExists == mergedExists && (!currentExists || reflect.DeepEqual(current, merged)) {
		return nil
	}
	kind := copierx.ChangeModify
	if !currentExists {
		kind = copierx.ChangeAdd
	} else if !mergedExists {
		kind = copierx.ChangeDelete
	}
	return []copierx.Change{{Path: path, Kind: kind}}
}
