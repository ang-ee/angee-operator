// Package inputform collects template answers through a terminal form, scripted
// line prompts, or defaults, using the same descriptor validation in each mode.
package inputform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ang-ee/angee-operator/api"
)

// Mode selects how template answers are collected.
type Mode int

const (
	ModeInteractive Mode = iota
	ModeScripted
	ModeDefaults
)

// Origin identifies the source of an input value.
type Origin string

const (
	OriginDefault  Origin = "default"
	OriginAnswers  Origin = "answers"
	OriginFlag     Origin = "flag"
	OriginRecorded Origin = "recorded"
	OriginChanged  Origin = "changed"
)

// Request describes the form and its initial answers. Input order is preserved.
type Request struct {
	Title    string
	Confirm  string
	Inputs   []api.TemplateInputDescriptor
	Provided map[string]string
	Origins  map[string]Origin
	Mode     Mode
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
}

// Result contains all question answers and any other provided keys.
type Result struct {
	Values  map[string]string
	Origins map[string]Origin
}

// ErrAborted indicates that the user declined or interrupted the form.
var ErrAborted = errors.New("aborted, nothing rendered")

// DetectMode gives explicit defaults priority over accessibility and TTY checks.
func DetectMode(yes, stdinIsTerminal bool, env func(string) string) Mode {
	if yes {
		return ModeDefaults
	}
	if !stdinIsTerminal || (env != nil && (env("ANGEE_ACCESSIBLE") == "1" || env("TERM") == "dumb")) {
		return ModeScripted
	}
	return ModeInteractive
}

// Run validates provided values before collecting and validating final answers.
func Run(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if req.In == nil {
		req.In = strings.NewReader("")
	}
	if req.Out == nil {
		req.Out = io.Discard
	}
	if req.Err == nil {
		req.Err = io.Discard
	}
	for _, desc := range req.Inputs {
		if value, ok := req.Provided[desc.Name]; ok {
			if err := Validate(desc, value); err != nil {
				// Defaults mode reports every missing required input together,
				// including explicit empty values. Other invalid provided values
				// fail before inspecting defaults or opening any prompts.
				var missing *requiredInputError
				if req.Mode == ModeDefaults && errors.As(err, &missing) {
					continue
				}
				return Result{}, err
			}
		}
	}
	switch req.Mode {
	case ModeDefaults:
		result := initialResult(req)
		if err := validateResult(req, result); err != nil {
			return Result{}, err
		}
		return result, nil
	case ModeInteractive:
		return runInteractive(ctx, req)
	case ModeScripted:
		return runScripted(ctx, req)
	default:
		return Result{}, fmt.Errorf("unknown template input form mode %d", req.Mode)
	}
}

func initialResult(req Request) Result {
	result := Result{Values: make(map[string]string), Origins: make(map[string]Origin)}
	for key, value := range req.Provided {
		result.Values[key] = value
		origin := req.Origins[key]
		if origin == "" {
			origin = OriginFlag
		}
		result.Origins[key] = origin
	}
	for _, desc := range req.Inputs {
		if !desc.Question || desc.Generated {
			continue
		}
		if _, ok := result.Values[desc.Name]; ok {
			continue
		}
		value := desc.Default
		if desc.Multiselect && value == "" {
			value = "[]"
		}
		result.Values[desc.Name] = value
		result.Origins[desc.Name] = OriginDefault
	}
	return result
}

func validateResult(req Request, result Result) error {
	var failures []error
	for _, desc := range req.Inputs {
		value, provided := req.Provided[desc.Name]
		if desc.Question && !desc.Generated {
			value = result.Values[desc.Name]
		} else if !provided {
			continue
		}
		// Historically an optional, unanswered line prompt accepts an empty
		// value. Explicit provided values still receive full type validation.
		if value == "" && !provided && !desc.Required && len(desc.Choices) == 0 && !desc.Multiselect {
			continue
		}
		if err := Validate(desc, value); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
