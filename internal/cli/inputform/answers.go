package inputform

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"gopkg.in/yaml.v3"
)

// LoadAnswersFile reads template inputs from a YAML mapping such as a Copier
// answers file. Keys starting with "_" (Copier metadata) are ignored. Scalars
// keep their source text verbatim, so dates, large integers, and zero-padded
// strings survive the round trip; null becomes an empty string. Lists use the
// form's JSON string-array encoding. A list that holds a mapping is a
// structured answer, as Copier records a `type: json` one (a repository list,
// say): it is encoded as the JSON it was, with YAML's own booleans, numbers
// and nulls. A mapping as an answer itself is rejected, and a key declared
// twice takes its last value.
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
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("answers file %s: key %s %w", path, key.Value, err)
			}
			answers[key.Value] = string(encoded)
		case string:
			answers[key.Value] = value
		}
	}
	return answers, nil
}

// answerValue converts a YAML node into the string (scalar) or []any (list)
// shape the answers map uses. A list of strings and lists keeps its members'
// text; one that holds a mapping, at any depth, is read whole as a
// structured answer (copierx.StructuredAnswer).
func answerValue(node *yaml.Node) (any, error) {
	node = resolveAlias(node)
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return "", nil
		}
		return node.Value, nil
	case yaml.SequenceNode:
		if copierx.AnswerHoldsMapping(node) {
			return copierx.StructuredAnswer(node)
		}
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
