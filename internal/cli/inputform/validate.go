package inputform

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ang-ee/angee-operator/api"
)

// Validate checks required values, descriptor types, and static choices. Raw
// Copier expressions in ChoicesExpr, When, and Validator remain informational.
func Validate(desc api.TemplateInputDescriptor, value string) error {
	if desc.Required && value == "" {
		return requiredError(desc.Name)
	}
	if desc.Multiselect {
		var values []*string
		if err := json.Unmarshal([]byte(value), &values); err != nil || values == nil {
			return fmt.Errorf("template input %s must be a JSON array of strings", desc.Name)
		}
		if desc.Required && len(values) == 0 {
			return requiredError(desc.Name)
		}
		for _, selection := range values {
			if selection == nil {
				return fmt.Errorf("template input %s must be a JSON array of strings", desc.Name)
			}
			if err := validateSingle(desc, *selection); err != nil {
				return err
			}
		}
		return nil
	}
	return validateSingle(desc, value)
}

func validateSingle(desc api.TemplateInputDescriptor, value string) error {
	if len(desc.Choices) > 0 {
		valid := false
		for _, choice := range desc.Choices {
			if value == choice.Value {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("template input %s must be one of: %s", desc.Name, strings.Join(choiceValues(desc), ", "))
		}
	}
	switch desc.Type {
	case "int", "integer":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("template input %s must be an integer", desc.Name)
		}
	case "bool", "boolean":
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("template input %s must be a boolean", desc.Name)
		}
	}
	return nil
}

func requiredError(name string) error {
	return &requiredInputError{name: name}
}

type requiredInputError struct{ name string }

func (err *requiredInputError) Error() string {
	return fmt.Sprintf("template input %s is required; pass --input %s=value", err.name, err.name)
}

func choiceValues(desc api.TemplateInputDescriptor) []string {
	values := make([]string, len(desc.Choices))
	for i, choice := range desc.Choices {
		values[i] = choice.Value
	}
	return values
}
