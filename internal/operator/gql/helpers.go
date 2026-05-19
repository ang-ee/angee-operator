package gql

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/operator/gql/model"
	"gopkg.in/yaml.v3"
)

func actionResult(status string) *model.MutationResult {
	return &model.MutationResult{Status: status}
}

func namedActionResult(status, name string) *model.MutationResult {
	return &model.MutationResult{Status: status, Name: &name}
}

func serviceRequestFrom(input model.ServiceInput) api.ServiceInitRequest {
	return api.ServiceInitRequest{
		Name:    stringPtrValue(input.Name),
		Runtime: stringPtrValue(input.Runtime),
		Image:   stringPtrValue(input.Image),
		Command: input.Command,
		Mounts:  input.Mounts,
		Env:     keyValuesFrom(input.Env),
		Ports:   input.Ports,
		Workdir: stringPtrValue(input.Workdir),
		Start:   boolPtrValue(input.Start),
	}
}

func workspaceCreateRequestFrom(input model.WorkspaceCreateInput) api.WorkspaceCreateRequest {
	return api.WorkspaceCreateRequest{
		Template: input.Template,
		Name:     stringPtrValue(input.Name),
		Inputs:   keyValuesFrom(input.Inputs),
		TTL:      stringPtrValue(input.TTL),
	}
}

func serviceCreateRequestFrom(input model.ServiceCreateInput) api.ServiceCreateRequest {
	return api.ServiceCreateRequest{
		Template:  input.Template,
		Workspace: input.Workspace,
		Name:      stringPtrValue(input.Name),
		Inputs:    keyValuesFrom(input.Inputs),
		Start:     boolPtrValue(input.Start),
	}
}

func stackRuntimeRequest(input *model.StackRuntimeInput) api.StackRuntimeRequest {
	if input == nil {
		return api.StackRuntimeRequest{}
	}
	return api.StackRuntimeRequest{Services: input.Services, Build: boolPtrValue(input.Build)}
}

func keyValuesFrom(values []*model.KeyValueInput) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, value := range values {
		if value != nil && value.Key != "" {
			out[value.Key] = value.Value
		}
	}
	return out
}

func keyValueList(values map[string]string) []*model.KeyValue {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*model.KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, &model.KeyValue{Key: key, Value: values[key]})
	}
	return out
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func logLimitValue(value *int) int {
	if value == nil || *value <= 0 || *value > maxGraphQLLogBytes {
		return maxGraphQLLogBytes
	}
	return *value
}

func collectLogStream(logs <-chan string, limit int) string {
	var out strings.Builder
	remaining := limit
	truncated := false
	for line := range logs {
		if remaining <= 0 {
			if !truncated {
				out.WriteString("\n[truncated]\n")
				truncated = true
			}
			continue
		}
		if len(line) > remaining {
			out.WriteString(line[:remaining])
			out.WriteString("\n[truncated]\n")
			remaining = 0
			truncated = true
			continue
		}
		out.WriteString(line)
		remaining -= len(line)
	}
	return out.String()
}

func formatGraphQLTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func yamlTaggedMap(value any) (map[string]any, error) {
	decoded, err := yamlTaggedValue(value)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, nil
	}
	out, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compiled value decoded as %T, want map", decoded)
	}
	return out, nil
}

func yamlTaggedValue(value any) (any, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return normalizeYAMLValue(decoded), nil
}

func normalizeYAMLValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range value {
			out[key] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for key, item := range value {
			out[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return value
	}
}

func sortedMapValues[T any](values map[string]T) []T {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, key := range keys {
		out = append(out, values[key])
	}
	return out
}

func ptrSlice[T any](values []T) []*T {
	out := make([]*T, 0, len(values))
	for i := range values {
		out = append(out, &values[i])
	}
	return out
}

func stringMapJSON(values map[string]string) map[string]any {
	if values == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func intMapJSON(values map[string]int) map[string]any {
	if values == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func anyMapJSON[T any](values map[string]T) map[string]any {
	if values == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func mcpDescriptor() map[string]any {
	return map[string]any{
		"name":    "angee-operator",
		"version": "0.1",
		"tools": []string{
			"stack.status",
			"stack.up",
			"stack.down",
			"services.create",
			"workspaces.create",
			"sources.fetch",
		},
	}
}
