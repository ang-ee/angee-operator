package copierx

import (
	"github.com/ang-ee/angee-operator/api"
	copier "github.com/fyltr/copier-go"
)

// InputApplies reports whether an input whose `when` condition is when applies
// given the answers so far, as copier decides it: copier-go renders the
// condition with the answers parsed to each input's type, and a false
// condition means the question is not asked and takes its default. An empty
// condition, or one that fails to render, applies (copier shows the question).
func InputApplies(when string, inputs []api.TemplateInputDescriptor, values map[string]string) bool {
	if when == "" {
		return true
	}
	answers := make(map[string]any, len(values))
	for _, desc := range inputs {
		raw, ok := values[desc.Name]
		if !ok {
			continue
		}
		answers[desc.Name] = raw
		if parsed, err := copier.ParseAnswer(copier.QuestionDef{Name: desc.Name, Type: copier.QuestionType(desc.Type)}, raw); err == nil {
			answers[desc.Name] = parsed
		}
	}
	for name, raw := range values {
		if _, ok := answers[name]; !ok {
			answers[name] = raw
		}
	}
	return copier.ShouldAsk(copier.QuestionDef{When: when}, copier.NewRenderer(nil, ""), answers)
}
