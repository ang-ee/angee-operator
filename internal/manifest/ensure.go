package manifest

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Ensure merges template-declared invariants into the stack manifest.
//
// Each key in the input map is a dotted path into the manifest tree
// (e.g. "operator.port_pool.django"). The associated value is grafted
// at that path under fail-on-different semantics:
//
//   - missing path  → set to value
//   - present, deep-equal value → no-op
//   - present, different value  → error
//
// The merge is performed against a YAML map representation of the stack
// and the result is unmarshalled back through Stack.Defaults and Stack.Validate, so any
// shape error surfaces immediately.
func Ensure(stack *Stack, requirements map[string]any) error {
	if len(requirements) == 0 {
		return nil
	}
	if stack == nil {
		return fmt.Errorf("ensure: stack is nil")
	}
	encoded, err := yaml.Marshal(stack)
	if err != nil {
		return fmt.Errorf("ensure: marshal stack: %w", err)
	}
	root := map[string]any{}
	if err := yaml.Unmarshal(encoded, &root); err != nil {
		return fmt.Errorf("ensure: unmarshal stack: %w", err)
	}
	for _, path := range sortedRequirementKeys(requirements) {
		if err := ensurePath(root, path, requirements[path]); err != nil {
			return err
		}
	}
	merged, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("ensure: marshal merged: %w", err)
	}
	var next Stack
	dec := yaml.NewDecoder(strings.NewReader(string(merged)))
	dec.KnownFields(true)
	if err := dec.Decode(&next); err != nil {
		return fmt.Errorf("ensure: decode merged: %w", err)
	}
	next.Defaults()
	if err := next.Validate(); err != nil {
		return fmt.Errorf("ensure: %w", err)
	}
	*stack = next
	return nil
}

func ensurePath(root map[string]any, path string, want any) error {
	segments := strings.Split(path, ".")
	if len(segments) == 0 || segments[0] == "" {
		return fmt.Errorf("ensure: empty path")
	}
	cursor := root
	for i, key := range segments[:len(segments)-1] {
		if key == "" {
			return fmt.Errorf("ensure: empty segment in %q", path)
		}
		next, ok := cursor[key]
		if !ok || next == nil {
			child := map[string]any{}
			cursor[key] = child
			cursor = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("ensure: %s: cannot descend through non-mapping at %q", path, strings.Join(segments[:i+1], "."))
		}
		cursor = child
	}
	leaf := segments[len(segments)-1]
	existing, present := cursor[leaf]
	if !present || existing == nil {
		cursor[leaf] = want
		return nil
	}
	if equalYAML(existing, want) {
		return nil
	}
	return fmt.Errorf("ensure: %s: existing value %v conflicts with required value %v", path, existing, want)
}

func equalYAML(a, b any) bool {
	left, err := yaml.Marshal(a)
	if err != nil {
		return false
	}
	right, err := yaml.Marshal(b)
	if err != nil {
		return false
	}
	var left2, right2 any
	if err := yaml.Unmarshal(left, &left2); err != nil {
		return false
	}
	if err := yaml.Unmarshal(right, &right2); err != nil {
		return false
	}
	return reflect.DeepEqual(left2, right2)
}

func sortedRequirementKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
