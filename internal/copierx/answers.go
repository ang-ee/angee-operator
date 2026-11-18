package copierx

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Copier records an answer of `type: json` (or `yaml`) as the YAML of its
// value. A recorded list that holds a mapping is such a structured answer,
// which the readers of an answers file pass on as the JSON it was; a list of
// scalars is a multiselect's, whose members they keep as text.

// AnswerHoldsMapping reports whether the list node of an answers file holds
// a mapping, in itself or in a list it holds: whether it is a structured
// answer, to be read with StructuredAnswer.
func AnswerHoldsMapping(node *yaml.Node) bool {
	for _, item := range node.Content {
		item = resolveAnswerAlias(item)
		if item.Kind == yaml.MappingNode || item.Kind == yaml.SequenceNode && AnswerHoldsMapping(item) {
			return true
		}
	}
	return false
}

// StructuredAnswer reads a node of a structured answer as the JSON value it
// records: a boolean, a number or a null as such, any other scalar as its
// text, a list as a []any and a mapping with plain keys as a map[string]any.
// Its errors read after the name of the answer ("key x holds …").
func StructuredAnswer(node *yaml.Node) (any, error) {
	node = resolveAnswerAlias(node)
	switch node.Kind {
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var value any
			if err := node.Decode(&value); err != nil {
				return nil, fmt.Errorf("holds a value that cannot be read: %w", err)
			}
			return value, nil
		}
		return node.Value, nil
	case yaml.SequenceNode:
		items := make([]any, len(node.Content))
		for i, item := range node.Content {
			var err error
			if items[i], err = StructuredAnswer(item); err != nil {
				return nil, err
			}
		}
		return items, nil
	case yaml.MappingNode:
		entries := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := resolveAnswerAlias(node.Content[i])
			if key.Kind != yaml.ScalarNode || key.Tag == "!!merge" {
				return nil, fmt.Errorf("holds a mapping whose keys are not plain scalars")
			}
			value, err := StructuredAnswer(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			entries[key.Value] = value
		}
		return entries, nil
	default:
		return nil, fmt.Errorf("has an unsupported YAML node kind")
	}
}

func resolveAnswerAlias(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}
