package inputform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ang-ee/angee-operator/api"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// drive executes the actual form commands, including nested batches. Cursor
// blink and spinner ticks are cosmetic, recurring messages; executing their
// follow-up commands forever would prevent the next keystroke. Virtual time
// lets their initial commands finish without slowing down keyboard tests.
func drive(t *testing.T, form *huh.Form, msgs ...tea.Msg) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var send func(tea.Msg)
		var execute func(tea.Cmd)
		steps := 0
		execute = func(cmd tea.Cmd) {
			if cmd != nil {
				send(cmd())
			}
		}
		send = func(msg tea.Msg) {
			if msg == nil {
				return
			}
			steps++
			if steps > 5000 {
				t.Fatal("form commands did not settle")
			}
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, cmd := range batch {
					execute(cmd)
				}
				return
			}
			pkg := reflect.TypeOf(msg).PkgPath()
			if pkg == "github.com/charmbracelet/bubbles/cursor" || pkg == "github.com/charmbracelet/bubbles/spinner" {
				return
			}
			_, cmd := form.Update(msg)
			execute(cmd)
		}
		execute(form.Init())
		send(tea.WindowSizeMsg{Width: 100, Height: 40})
		for _, msg := range msgs {
			send(msg)
		}
	})
}

func TestInteractiveKeyboardNavigation(t *testing.T) {
	var summary bytes.Buffer
	req := Request{
		Title: "Initialize stack stacks/dev", Err: &summary,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "project", Question: true, Required: true, Help: "Name of the project."},
			{Name: "runtime", Question: true, Default: "process", Choices: []api.TemplateInputChoice{
				{Value: "process", Label: "Local processes"}, {Value: "docker", Label: "Docker containers"},
			}},
			{Name: "enabled", Type: "bool", Question: true, Default: "false"},
		},
	}
	form, bound := build(req)
	drive(t, form,
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("notes")},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyDown},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyLeft},
		tea.KeyMsg{Type: tea.KeyShiftTab},
		tea.KeyMsg{Type: tea.KeyShiftTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("-app")},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEnter},
	)
	if form.State != huh.StateCompleted {
		t.Fatalf("form did not complete: %v\n%s", form.State, form.View())
	}
	result, err := finishInteractive(req, bound, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantValues := map[string]string{"project": "notes-app", "runtime": "docker", "enabled": "true"}
	wantOrigins := map[string]Origin{"project": OriginChanged, "runtime": OriginChanged, "enabled": OriginChanged}
	if !reflect.DeepEqual(result.Values, wantValues) || !reflect.DeepEqual(result.Origins, wantOrigins) {
		t.Fatalf("got %#v, want values %#v and origins %#v", result, wantValues, wantOrigins)
	}
	wantSummary := "inputs for Initialize stack stacks/dev:\n  project = notes-app (changed)\n  runtime = docker (changed)\n  enabled = true (changed)\n"
	if summary.String() != wantSummary {
		t.Fatalf("summary = %q, want %q", summary.String(), wantSummary)
	}
}

func TestInteractiveScreenAndOrigins(t *testing.T) {
	var summary bytes.Buffer
	req := Request{
		Title: "Initialize stack stacks/dev", Confirm: "Re-render the stack?", Err: &summary,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "name", Question: true, Required: true, Default: "notes", Help: "Project name"},
			{Name: "answer", Question: true},
			{Name: "flag", Question: true},
			{Name: "recorded", Question: true},
			{Name: "api_key", Question: true, Secret: true, Default: "hidden-default"},
			{Name: "locked", Question: true, Immutable: true, Default: "fixed"},
			{Name: "token", Generated: true, Secret: true, Default: "hidden-generated"},
			{Name: "internal", Default: "internal-default"},
		},
		Provided: map[string]string{"answer": "answer", "flag": "flag", "recorded": "old", "extra": "passthrough"},
		Origins:  map[string]Origin{"answer": OriginAnswers, "recorded": OriginRecorded},
	}
	form, bound := build(req)
	drive(t, form)
	view := form.View()
	for _, want := range []string{"Initialize stack stacks/dev", "name (required)", "Project name", "(default)", "(answers)", "(flag)", "(recorded)"} {
		if !strings.Contains(view, want) {
			t.Errorf("screen missing %q:\n%s", want, view)
		}
	}
	if lipgloss.Width(view) > 100 {
		t.Errorf("screen width %d exceeds terminal width", lipgloss.Width(view))
	}
	drive(t, form, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !strings.Contains(form.View(), "(changed)") {
		t.Fatalf("edited origin missing:\n%s", form.View())
	}
	drive(t, form, tea.KeyMsg{Type: tea.KeyBackspace})
	if bound.fields[0].currentOrigin() != OriginDefault {
		t.Fatal("restoring the starting value should restore its origin")
	}
	drive(t, form,
		tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyTab},
	)
	view = form.View()
	for _, want := range []string{"locked = fixed (immutable)", "token = ******** (generated)", "internal = internal-default (read-only)", "Re-render the stack?"} {
		if !strings.Contains(view, want) {
			t.Errorf("screen missing %q:\n%s", want, view)
		}
	}
	for _, secret := range []string{"hidden-default", "hidden-generated"} {
		if strings.Contains(view, secret) {
			t.Errorf("screen exposed %q", secret)
		}
	}
	drive(t, form, tea.KeyMsg{Type: tea.KeyEnter})
	result, err := finishInteractive(req, bound, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Values["extra"] != "passthrough" || result.Values["locked"] != "fixed" || result.Origins["recorded"] != OriginRecorded || result.Origins["answer"] != OriginAnswers {
		t.Fatalf("lost initial values or origins: %#v", result)
	}
	if strings.Contains(summary.String(), "hidden-") || !strings.Contains(summary.String(), "  api_key = ******** (default)\n") {
		t.Fatalf("bad secret summary: %s", &summary)
	}
}

func TestInteractiveMultiselect(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []tea.Msg
		want string
	}{
		{name: "empty", want: "[]"},
		{name: "selected", keys: []tea.Msg{tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace}}, want: `["b"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Inputs: []api.TemplateInputDescriptor{{Name: "addons", Question: true, Multiselect: true,
				Choices: []api.TemplateInputChoice{{Value: "a", Label: "Alpha"}, {Value: "b", Label: "Beta"}},
			}}}
			form, bound := build(req)
			tc.keys = append(tc.keys, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter})
			drive(t, form, tc.keys...)
			result, confirmed := bound.collect()
			if form.State != huh.StateCompleted || !confirmed || result.Values["addons"] != tc.want {
				t.Fatalf("state=%v, confirmed=%v, result=%#v", form.State, confirmed, result)
			}
		})
	}
}

func TestInteractiveRejectAndAbort(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []tea.Msg
	}{
		{name: "no", keys: []tea.Msg{tea.KeyMsg{Type: tea.KeyLeft}, tea.KeyMsg{Type: tea.KeyEnter}}},
		{name: "ctrl-c", keys: []tea.Msg{tea.KeyMsg{Type: tea.KeyCtrlC}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var summary bytes.Buffer
			req := Request{Err: &summary}
			form, bound := build(req)
			drive(t, form, tc.keys...)
			var runErr error
			if form.State == huh.StateAborted {
				runErr = huh.ErrUserAborted
			}
			_, err := finishInteractive(req, bound, runErr)
			if !errors.Is(err, ErrAborted) || summary.Len() != 0 {
				t.Fatalf("err=%v, summary=%q", err, &summary)
			}
		})
	}
}

func TestInteractiveMultiselectPrefill(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desc    api.TemplateInputDescriptor
		initial string
		want    string
	}{
		{name: "dynamic choices", desc: api.TemplateInputDescriptor{ChoicesExpr: "{{ options }}"}, initial: `["one","two"]`, want: `["one","two"]`},
		{name: "static choice order", desc: api.TemplateInputDescriptor{Choices: []api.TemplateInputChoice{{Value: "a"}, {Value: "b"}}}, initial: `["b", "a"]`, want: `["a","b"]`},
		{name: "boolean selections", desc: api.TemplateInputDescriptor{Type: "bool", Choices: []api.TemplateInputChoice{{Value: "true"}, {Value: "false"}}}, initial: `["true","false"]`, want: `["true","false"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.desc.Name, tc.desc.Question, tc.desc.Multiselect, tc.desc.Required = "features", true, true, true
			req := Request{Inputs: []api.TemplateInputDescriptor{tc.desc}, Provided: map[string]string{"features": tc.initial}, Err: io.Discard}
			form, bound := build(req)
			drive(t, form, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter})
			result, err := finishInteractive(req, bound, nil)
			if err != nil || form.State != huh.StateCompleted || result.Values["features"] != tc.want || result.Origins["features"] != OriginFlag {
				t.Fatalf("state=%v, result=%#v, err=%v", form.State, result, err)
			}
		})
	}
}

func TestInteractiveIntegerValidation(t *testing.T) {
	req := Request{Inputs: []api.TemplateInputDescriptor{{Name: "port", Type: "int", Question: true, Required: true}}, Err: io.Discard}
	form, bound := build(req)
	drive(t, form, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("oops")}, tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(form.View(), "template input port must be an integer") {
		t.Fatalf("missing inline validation error:\n%s", form.View())
	}
	drive(t, form, tea.KeyMsg{Type: tea.KeyCtrlA}, tea.KeyMsg{Type: tea.KeyCtrlK},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("8080")}, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter})
	result, err := finishInteractive(req, bound, nil)
	if err != nil || form.State != huh.StateCompleted || result.Values["port"] != "8080" {
		t.Fatalf("state=%v, result=%#v, err=%v", form.State, result, err)
	}
}

func TestInteractiveRunRejectsNo(t *testing.T) {
	// Exercise Run's stream wiring and ErrAborted mapping without a real TTY.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var summary bytes.Buffer
	_, err := Run(ctx, Request{Mode: ModeInteractive, In: strings.NewReader("\x1b[D\r"), Out: io.Discard, Err: &summary})
	if ctx.Err() != nil {
		t.Fatal("form ignored the supplied input and timed out")
	}
	if !errors.Is(err, ErrAborted) || summary.Len() != 0 {
		t.Fatalf("err=%v, summary=%q", err, &summary)
	}
}

func TestInteractiveRunUpdatesDescriptions(t *testing.T) {
	// Unlike drive, Run executes dynamic descriptions on Bubble Tea command
	// goroutines. Many separate key events exercise concurrent reads and writes
	// under -race; tabs delimit fields without introducing line-prompt behavior.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var summary bytes.Buffer
	input := strings.Repeat("x\x1b[D", 100) + "\t\r"
	result, err := Run(ctx, Request{
		Mode: ModeInteractive, In: strings.NewReader(input), Out: io.Discard, Err: &summary,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "project", Question: true, Required: true, Help: "Project name"},
		},
	})
	if ctx.Err() != nil || err != nil {
		t.Fatalf("Run failed: context=%v, err=%v", ctx.Err(), err)
	}
	if result.Values["project"] != strings.Repeat("x", 100) {
		t.Fatalf("unexpected answers: %#v", result)
	}
	for _, origin := range result.Origins {
		if origin != OriginChanged {
			t.Fatalf("unexpected origins: %#v", result.Origins)
		}
	}
}

func TestInteractiveSmallTerminalPaging(t *testing.T) {
	req := Request{Title: "Small terminal", Inputs: []api.TemplateInputDescriptor{
		{Name: "one", Question: true}, {Name: "two", Question: true},
		{Name: "three", Question: true}, {Name: "four", Question: true},
		{Name: "five", Question: true}, {Name: "six", Question: true},
	}}
	form, bound := buildWithHeight(req, 14)
	drive(t, form)
	if strings.Contains(form.View(), "five") {
		t.Fatalf("fifth input should be on the second page:\n%s", form.View())
	}
	drive(t, form,
		tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fifth")},
		tea.KeyMsg{Type: tea.KeyShiftTab}, tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyEnter},
		tea.KeyMsg{Type: tea.KeyEnter},
	)
	result, confirmed := bound.collect()
	if form.State != huh.StateCompleted || !confirmed || result.Values["five"] != "fifth" {
		t.Fatalf("state=%v, confirmed=%v, result=%#v", form.State, confirmed, result)
	}
}
