package inputform

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ang-ee/angee-operator/api"
	"github.com/charmbracelet/x/term"
)

// promptAttempts bounds invalid answers before returning the validation error.
const promptAttempts = 3

func runScripted(ctx context.Context, req Request) (Result, error) {
	result := initialResult(req)
	reader := bufio.NewReader(req.In)
	for _, desc := range req.Inputs {
		if !desc.Question || desc.Generated || desc.Immutable {
			continue
		}
		if _, provided := req.Provided[desc.Name]; provided {
			continue
		}
		desc.Default = result.Values[desc.Name]
		value, err := prompt(ctx, req, reader, desc)
		if err != nil {
			return Result{}, err
		}
		if value != result.Values[desc.Name] {
			result.Origins[desc.Name] = OriginChanged
		}
		result.Values[desc.Name] = value
	}
	if err := validateResult(req, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// A plain reader cannot suppress terminal echo. Secret answers and defaults are
// therefore kept out of prompts and never printed back by the scripted mode.
func prompt(ctx context.Context, req Request, reader *bufio.Reader, desc api.TemplateInputDescriptor) (string, error) {
	label := desc.Name + ": "
	if !desc.Secret {
		switch desc.Type {
		case "bool", "boolean":
			label = desc.Name + " [y/N]: "
			if defaultTrue, _ := strconv.ParseBool(desc.Default); defaultTrue {
				label = desc.Name + " [Y/n]: "
			}
		default:
			if desc.Default != "" {
				label = fmt.Sprintf("%s [%s]: ", desc.Name, desc.Default)
			}
		}
	}
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			// Interrupted mid-prompt: same outcome as ctrl+c in the form.
			return "", ErrAborted
		}
		if _, err := fmt.Fprint(req.Err, WrappedHelp(desc.Help, 2)); err != nil {
			return "", err
		}
		if len(desc.Choices) > 0 {
			if _, err := fmt.Fprintf(req.Err, "  choices: %s\n", strings.Join(choiceValues(desc), " | ")); err != nil {
				return "", err
			}
		}
		if _, err := fmt.Fprint(req.Err, label); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", fmt.Errorf("template input %s requires interactive input; use --yes to accept defaults or --input %s=value", desc.Name, desc.Name)
		}
		// Scripted readers do not echo Enter. Keep help and warnings on their
		// own lines, while avoiding a duplicate newline on a real terminal.
		if req.In != os.Stdin || !term.IsTerminal(os.Stdin.Fd()) {
			if _, err := fmt.Fprintln(req.Err); err != nil {
				return "", err
			}
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = desc.Default
		}
		if desc.Type == "bool" || desc.Type == "boolean" {
			switch strings.ToLower(value) {
			case "y", "yes":
				value = "true"
			case "n", "no":
				value = "false"
			}
		}
		var validationErr error
		if value != "" || desc.Required || len(desc.Choices) > 0 || desc.Multiselect {
			validationErr = Validate(desc, value)
		}
		if validationErr == nil {
			return value, nil
		}
		if _, err := fmt.Fprintf(req.Err, "warning: %s\n", validationErr); err != nil {
			return "", err
		}
		if attempt == promptAttempts {
			return "", validationErr
		}
	}
}

// WrappedHelp preserves explicit line breaks and wraps text to 78 columns,
// including the indentation used by prompts and template descriptions.
func WrappedHelp(help string, indent int) string {
	if strings.TrimSpace(help) == "" {
		return ""
	}
	prefix := strings.Repeat(" ", indent)
	var out strings.Builder
	for _, paragraph := range strings.Split(strings.TrimSpace(help), "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			if line != "" && indent+utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > 78 {
				out.WriteString(prefix + line + "\n")
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		out.WriteString(prefix + line + "\n")
	}
	return out.String()
}
