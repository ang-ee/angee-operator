package inputform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ang-ee/angee-operator/api"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
)

type fieldBinding struct {
	mu      sync.RWMutex
	desc    api.TemplateInputDescriptor
	start   string
	origin  Origin
	text    *string
	boolean *bool
	multi   *[]string
}

func (b *fieldBinding) value() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	switch {
	case b.boolean != nil:
		return strconv.FormatBool(*b.boolean)
	case b.multi != nil:
		return selectionJSON(*b.multi)
	default:
		return *b.text
	}
}

// valueAccessor keeps the bound pointers available to huh's event-loop change
// detection, while synchronizing writes with its asynchronous DescriptionFunc
// callbacks. Slice values are replaced by MultiSelect, never modified in place.
type valueAccessor[T any] struct {
	value *T
	mu    *sync.RWMutex
}

func (a valueAccessor[T]) Get() T {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return *a.value
}

func (a valueAccessor[T]) Set(value T) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*a.value = value
}

func (b *fieldBinding) currentOrigin() Origin {
	if b.value() != b.start {
		return OriginChanged
	}
	return b.origin
}

func (b *fieldBinding) description() string {
	marker := "(" + string(b.currentOrigin()) + ")"
	if b.desc.Help != "" {
		return b.desc.Help + "\n" + marker
	}
	return marker
}

type bindings struct {
	initial Result
	fields  []*fieldBinding
	confirm bool
}

func (b *bindings) collect() (Result, bool) {
	result := Result{Values: make(map[string]string), Origins: make(map[string]Origin)}
	for key, value := range b.initial.Values {
		result.Values[key] = value
		result.Origins[key] = b.initial.Origins[key]
	}
	for _, field := range b.fields {
		result.Values[field.desc.Name] = field.value()
		result.Origins[field.desc.Name] = field.currentOrigin()
	}
	return result, b.confirm
}

// build keeps the form independent of a running terminal, so keyboard navigation
// can also be exercised through the Bubble Tea model's Init and Update methods.
func build(req Request) (*huh.Form, *bindings) {
	height := 0
	if out, ok := req.Out.(*os.File); ok && term.IsTerminal(out.Fd()) {
		if _, h, err := term.GetSize(out.Fd()); err == nil {
			height = h
		}
	}
	return buildWithHeight(req, height)
}

func buildWithHeight(req Request, height int) (*huh.Form, *bindings) {
	b := &bindings{initial: initialResult(req), confirm: true}
	var fields []huh.Field
	var readOnly []string
	for _, desc := range req.Inputs {
		value := desc.Default
		if provided, ok := req.Provided[desc.Name]; ok {
			value = provided
		}
		if !desc.Question || desc.Generated || desc.Immutable {
			kind := "read-only"
			if desc.Generated {
				kind = "generated"
			} else if desc.Immutable {
				kind = "immutable"
			}
			if desc.Secret {
				value = "********"
			}
			readOnly = append(readOnly, fmt.Sprintf("%s = %s (%s)", desc.Name, value, kind))
			continue
		}
		binding, field := newField(desc, b.initial.Values[desc.Name], b.initial.Origins[desc.Name])
		b.fields = append(b.fields, binding)
		fields = append(fields, field)
	}

	intro := huh.NewNote().Title(req.Title).Description(fmt.Sprintf(
		"%d inputs · tab/enter next · shift+tab back · ↑↓ choose · ←→ toggle · ctrl+c abort", len(fields)))
	fields = append([]huh.Field{intro}, fields...)
	if len(readOnly) > 0 {
		// Notes interpret Markdown. Escape names, paths, and secret masks so
		// the read-only values appear literally instead of becoming markup.
		escape := strings.NewReplacer("\\", "\\\\", "_", "\\_", "*", "\\*", "`", "\\`")
		fields = append(fields, huh.NewNote().Description(escape.Replace(strings.Join(readOnly, "\n"))))
	}
	confirm := req.Confirm
	if confirm == "" {
		confirm = "Render the template?"
	}
	fields = append(fields, huh.NewConfirm().Title(confirm).Affirmative("Yes").Negative("No").Value(&b.confirm))

	var groups []*huh.Group
	pageSize := len(fields)
	if height > 0 && height < 15 {
		pageSize = 5
	}
	for len(fields) > 0 {
		count := min(pageSize, len(fields))
		groups = append(groups, huh.NewGroup(fields[:count]...))
		fields = fields[count:]
	}
	// Angee chooses accessibility itself: huh's accessible runner bypasses the
	// supplied reader and reads os.Stdin directly.
	form := huh.NewForm(groups...).WithAccessible(false).WithShowHelp(true).
		WithInput(req.In).WithOutput(req.Out)
	if height > 0 {
		form.WithHeight(height)
	}
	return form, b
}

func newField(desc api.TemplateInputDescriptor, value string, origin Origin) (*fieldBinding, huh.Field) {
	b := &fieldBinding{desc: desc, origin: origin}
	title := desc.Name
	if desc.Required {
		title += " (required)"
	}
	options := make([]huh.Option[string], len(desc.Choices))
	for i, choice := range desc.Choices {
		label := choice.Label
		if label == "" {
			label = choice.Value
		}
		options[i] = huh.NewOption(label, choice.Value)
	}
	validate := func(value string) error {
		// Match the scripted and defaults modes: an optional single-value
		// field without choices may be left blank.
		if value == "" && !desc.Required && len(desc.Choices) == 0 && !desc.Multiselect {
			return nil
		}
		return Validate(desc, value)
	}
	switch {
	case !desc.Multiselect && (desc.Type == "bool" || desc.Type == "boolean"):
		initial, _ := strconv.ParseBool(value)
		b.boolean = &initial
		b.start = b.value()
		field := huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Accessor(valueAccessor[bool]{b.boolean, &b.mu}).
			Description(b.description()).DescriptionFunc(b.description, b.boolean).
			Validate(func(value bool) error { return validate(strconv.FormatBool(value)) })
		return b, field
	case desc.Multiselect && len(options) > 0:
		initial := []string{}
		// Provided values have already been validated by Run. An invalid
		// template default starts empty so the user can select valid values.
		if err := json.Unmarshal([]byte(value), &initial); err != nil {
			initial = []string{}
		}
		// huh serializes selections in option order. Normalize the starting
		// selection to that same order so merely accepting it retains its origin.
		selected := make(map[string]bool, len(initial))
		for _, item := range initial {
			selected[item] = true
		}
		initial = []string{}
		for _, choice := range desc.Choices {
			if selected[choice.Value] {
				initial = append(initial, choice.Value)
			}
		}
		b.multi = &initial
		b.start = b.value()
		field := huh.NewMultiSelect[string]().Title(title).Options(options...).Accessor(valueAccessor[[]string]{b.multi, &b.mu}).
			Description(b.description()).DescriptionFunc(b.description, b.multi).
			Validate(func(value []string) error { return validate(selectionJSON(value)) })
		return b, field
	case len(options) > 0:
		b.text = &value
		b.start = value
		field := huh.NewSelect[string]().Title(title).Options(options...).Accessor(valueAccessor[string]{b.text, &b.mu}).
			Filtering(len(options) > 8).Description(b.description()).DescriptionFunc(b.description, b.text).
			Validate(validate)
		return b, field
	default:
		// Without static options (for example ChoicesExpr), a multiselect
		// uses JSON text. A zero-option MultiSelect would erase existing values
		// on Enter and could never satisfy a required selection.
		b.text = &value
		b.start = value
		field := huh.NewInput().Title(title).Accessor(valueAccessor[string]{b.text, &b.mu}).
			Description(b.description()).DescriptionFunc(b.description, b.text).Validate(validate)
		if desc.Secret {
			field.EchoMode(huh.EchoModePassword)
		} else {
			placeholder := desc.Placeholder
			if placeholder == "" {
				placeholder = desc.Default
			}
			field.Placeholder(placeholder)
		}
		return b, field
	}
}

func selectionJSON(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	// A slice of strings always marshals successfully.
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// The first frame waits for lipgloss's background-colour query (termenv's
// OSC 11 round trip, 5s timeout). Real terminals answer within milliseconds
// and termenv skips the query under screen/tmux/dumb TERMs, so only a
// terminal that swallows OSC replies pays the timeout.
func runInteractive(ctx context.Context, req Request) (Result, error) {
	form, b := build(req)
	// WithProgramOptions replaces existing options in huh v0.6.0, so set the
	// streams afterwards. RunWithContext also prevents huh from replacing ctx
	// with the background context used by Run.
	form.WithProgramOptions(tea.WithContext(ctx)).WithInput(req.In).WithOutput(req.Out)
	err := form.RunWithContext(ctx)
	if ctx.Err() != nil {
		return Result{}, ErrAborted
	}
	return finishInteractive(req, b, err)
}

func finishInteractive(req Request, b *bindings, runErr error) (Result, error) {
	if errors.Is(runErr, huh.ErrUserAborted) {
		return Result{}, ErrAborted
	}
	if runErr != nil {
		return Result{}, runErr
	}
	result, confirmed := b.collect()
	if !confirmed {
		return Result{}, ErrAborted
	}
	if err := validateResult(req, result); err != nil {
		return Result{}, err
	}
	if _, err := fmt.Fprintf(req.Err, "inputs for %s:\n", req.Title); err != nil {
		return Result{}, err
	}
	for _, desc := range req.Inputs {
		if !desc.Question || desc.Generated {
			continue
		}
		value := result.Values[desc.Name]
		if desc.Secret {
			value = "********"
		}
		if _, err := fmt.Fprintf(req.Err, "  %s = %s (%s)\n", desc.Name, value, result.Origins[desc.Name]); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}
