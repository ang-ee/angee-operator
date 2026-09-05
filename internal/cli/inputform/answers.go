package inputform

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadAnswersFile reads template inputs from a YAML mapping such as a Copier
// answers file. Keys starting with "_" (Copier metadata) are ignored. Scalars
// keep their source text verbatim, so dates, large integers, and zero-padded
// strings survive the round trip; null becomes an empty string. Lists use the
// form's JSON string-array encoding. Nested mappings are rejected, and a key
// declared twice takes its last value.
func LoadAnswersFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("answers file %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("answers file %s: %w", path, err)
	}
	answers := make(map[string]string)
	if len(document.Content) == 0 {
		return answers, nil
	}
	root := resolveAlias(document.Content[0])
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("answers file %s: expected a YAML mapping", path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := resolveAlias(root.Content[i])
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("answers file %s: line %d: keys must be scalars", path, key.Line)
		}
		if strings.HasPrefix(key.Value, "_") || key.Tag == "!!merge" {
			continue
		}
		value, err := answerValue(root.Content[i+1])
		if err != nil {
			return nil, fmt.Errorf("answers file %s: key %s %w", path, key.Value, err)
		}
		switch value := value.(type) {
		case []any:
			// answerValue leaves only strings and lists, which always marshal.
			encoded, _ := json.Marshal(value)
			answers[key.Value] = string(encoded)
		case string:
			answers[key.Value] = value
		}
	}
	return answers, nil
}

// answerValue converts a YAML node into the string (scalar) or []any (list of
// strings and lists) shape the answers map uses.
func answerValue(node *yaml.Node) (any, error) {
	node = resolveAlias(node)
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
			items[i], err = answerValue(item)
			if err != nil {
				return nil, err
			}
		}
		return items, nil
	case yaml.MappingNode:
		return nil, fmt.Errorf("is a mapping; only scalars and lists are supported")
	default:
		return nil, fmt.Errorf("has an unsupported YAML node kind")
	}
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}
