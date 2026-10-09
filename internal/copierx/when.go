package copierx

import (
	"errors"
	"maps"

	"github.com/ang-ee/angee-operator/api"
	copier "github.com/fyltr/copier-go"
)

// InputApplies reports whether an input whose `when` condition is when applies
// given the answers so far, as copier decides it: copier-go evaluates the
// condition (EvaluateWhen) with the answers parsed to each input's type, and a
// false condition means the question is not asked and takes its default. An
// empty condition applies. One that fails to render applies too, so the
// question stays visible and the render reports the error.
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
		if parsed, err := copier.ParseAnswer(copier.QuestionDef{Name: desc.Name, Type: desc.Type}, raw); err == nil {
			answers[desc.Name] = parsed
		}
	}
	for name, raw := range values {
		if _, ok := answers[name]; !ok {
			answers[name] = raw
		}
	}
	applies, err := copier.EvaluateWhen(copier.QuestionDef{When: when}, answers)
	if err != nil {
		return true
	}
	return applies
}

// IsUnsupportedTemplate reports whether err is copier refusing a template
// whose _min_copier_version is newer than the Copier it implements.
func IsUnsupportedTemplate(err error) bool {
	return errors.Is(err, copier.ErrUnsupportedVersion)
}

// IsAnswerError reports whether err is copier refusing an answer before it
// renders anything: a validator rejecting it, a value outside the question's
// choices, or a required question with neither an input nor a default.
func IsAnswerError(err error) bool {
	var validation *copier.ValidationError
	var choice *copier.InvalidChoiceError
	return errors.As(err, &validation) || errors.As(err, &choice) ||
		errors.Is(err, copier.ErrInvalidChoice) || errors.Is(err, copier.ErrQuestionRequired)
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
		context := map[string]string{}
		maps.Copy(context, provided)
		maps.Copy(context, settled)
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
