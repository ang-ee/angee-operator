package inputform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

func TestDefaultsFillsQuestionsAndPreservesProvided(t *testing.T) {
	provided := map[string]string{"project": "notes", "runtime": "docker", "extra": "keep", "token": "recorded-token"}
	origins := map[string]Origin{"project": OriginAnswers, "token": OriginRecorded}
	var out bytes.Buffer
	result, err := Run(context.Background(), Request{
		Mode: ModeDefaults, In: failReader{t: t}, Out: &out, Err: &out,
		Provided: provided, Origins: origins,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "project", Question: true, Default: "app"},
			{Name: "runtime", Question: true, Default: "process", Choices: []api.TemplateInputChoice{{Value: "process"}, {Value: "docker"}}},
			{Name: "port", Question: true, Type: "int", Default: "8080"},
			{Name: "features", Question: true, Multiselect: true},
			{Name: "fixed", Question: true, Immutable: true, Default: "fixed-default"},
			{Name: "empty", Question: true},
			{Name: "token", Question: true, Secret: true},
			{Name: "generated", Question: true, Generated: true, Required: true},
			{Name: "metadata", Required: true, Default: "not-an-answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantValues := map[string]string{
		"project": "notes", "runtime": "docker", "extra": "keep", "port": "8080",
		"features": "[]", "fixed": "fixed-default", "empty": "", "token": "recorded-token",
	}
	wantOrigins := map[string]Origin{
		"project": OriginAnswers, "runtime": OriginFlag, "extra": OriginFlag, "port": OriginDefault,
		"features": OriginDefault, "fixed": OriginDefault, "empty": OriginDefault, "token": OriginRecorded,
	}
	if !reflect.DeepEqual(result.Values, wantValues) || !reflect.DeepEqual(result.Origins, wantOrigins) {
		t.Fatalf("result = %#v, want values %#v, origins %#v", result, wantValues, wantOrigins)
	}
	if out.Len() != 0 {
		t.Fatalf("defaults printed output: %q", out.String())
	}
	result.Values["project"] = "edited"
	result.Origins["project"] = OriginChanged
	if provided["project"] != "notes" || origins["project"] != OriginAnswers {
		t.Fatal("Run reused the caller's maps")
	}
}

func TestDefaultsReportsEveryMissingRequiredFlag(t *testing.T) {
	_, err := Run(context.Background(), Request{
		Mode: ModeDefaults, Provided: map[string]string{"provided_empty": ""},
		Inputs: []api.TemplateInputDescriptor{
			{Name: "api_key", Question: true, Required: true},
			{Name: "port", Question: true, Type: "int", Required: true},
			{Name: "fixed", Question: true, Immutable: true, Required: true},
			{Name: "provided_empty", Question: true, Required: true},
		},
	})
	want := "template input api_key is required; pass --input api_key=value\n" +
		"template input port is required; pass --input port=value\n" +
		"template input fixed is required; pass --input fixed=value\n" +
		"template input provided_empty is required; pass --input provided_empty=value"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestProvidedInputsValidatedBeforePrompts(t *testing.T) {
	for _, mode := range []Mode{ModeDefaults, ModeScripted, ModeInteractive} {
		for _, desc := range []api.TemplateInputDescriptor{
			{Name: "port", Type: "int", Question: true},
			{Name: "port", Type: "int", Generated: true},
			{Name: "port", Type: "int"},
		} {
			var out bytes.Buffer
			_, err := Run(context.Background(), Request{
				Mode: mode, Provided: map[string]string{"port": "invalid"},
				Inputs: []api.TemplateInputDescriptor{desc},
				In:     failReader{t: t}, Out: &out, Err: &out,
			})
			if err == nil || err.Error() != "template input port must be an integer" {
				t.Fatalf("mode %d, descriptor %#v: error = %v", mode, desc, err)
			}
			if out.Len() != 0 {
				t.Fatalf("mode %d printed before validating: %q", mode, out.String())
			}
		}
	}
}

func TestDetectMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		yes, tty   bool
		accessible string
		term       string
		want       Mode
	}{
		{name: "yes overrides all", yes: true, accessible: "1", term: "dumb", want: ModeDefaults},
		{name: "terminal", tty: true, term: "xterm-256color", want: ModeInteractive},
		{name: "piped stdin", want: ModeScripted},
		{name: "accessible", tty: true, accessible: "1", want: ModeScripted},
		{name: "dumb terminal", tty: true, term: "dumb", want: ModeScripted},
		{name: "accessible zero", tty: true, accessible: "0", want: ModeInteractive},
		{name: "accessible literal true is not one", tty: true, accessible: "true", want: ModeInteractive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := func(key string) string {
				return map[string]string{"ANGEE_ACCESSIBLE": tc.accessible, "TERM": tc.term}[key]
			}
			if got := DetectMode(tc.yes, tc.tty, env); got != tc.want {
				t.Fatalf("DetectMode = %v, want %v", got, tc.want)
			}
		})
	}
	if got := DetectMode(false, true, nil); got != ModeInteractive {
		t.Fatalf("DetectMode with no environment = %v", got)
	}
}

func TestRunCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, mode := range []Mode{ModeDefaults, ModeScripted, ModeInteractive} {
		_, err := Run(ctx, Request{Mode: mode, In: failReader{t: t}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("mode %d: error = %v, want context.Canceled", mode, err)
		}
	}
}

func TestRunUnknownMode(t *testing.T) {
	_, err := Run(context.Background(), Request{Mode: Mode(99)})
	if err == nil || !strings.Contains(err.Error(), "unknown template input form mode 99") {
		t.Fatalf("error = %v", err)
	}
}

type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("unexpected input read")
	return 0, io.EOF
}
