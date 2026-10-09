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

// SettleInputs walks inputs in order, as copier asks its questions, and
// reports which apply and the answer each settles to. A question's `when`
// sees the explicitly provided values (copier's data, visible to every
// question) and the settled answers of the questions before it, never a
// later question's default. A question that applies settles to its value in
// values, if any; one that does not settles to its default. An input that is
// not a question keeps its value.
func SettleInputs(inputs []api.TemplateInputDescriptor, values, provided map[string]string) (settled map[string]string, active map[string]bool) {
	settled = make(map[string]string, len(inputs))
	active = make(map[string]bool, len(inputs))
	for _, desc := range inputs {
		context := make(map[string]string, len(provided)+len(settled))
		for key, value := range provided {
			context[key] = value
		}
		for key, value := range settled {
			context[key] = value
		}
		applies := !desc.Question || InputApplies(desc.When, inputs, context)
		active[desc.Name] = applies
		if value, ok := values[desc.Name]; ok && applies {
			settled[desc.Name] = value
			continue
		}
		if desc.Question && !desc.Generated {
			value := desc.Default
			if desc.Multiselect && value == "" {
				value = "[]"
			}
			settled[desc.Name] = value
		}
	}
	return settled, active
}
