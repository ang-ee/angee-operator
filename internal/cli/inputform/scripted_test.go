package inputform

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ang-ee/angee-operator/api"
)

func TestScriptedHelpChoicesAndRetry(t *testing.T) {
	var output bytes.Buffer
	result, err := Run(context.Background(), Request{
		Mode: ModeScripted, In: strings.NewReader("\ninvalid\ndocker\n"), Err: &output,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "project", Question: true, Default: "notes", Help: "Name of the project."},
			{Name: "runtime", Question: true, Default: "process", Help: "How services run.",
				Choices: []api.TemplateInputChoice{{Value: "process", Label: "Local processes"}, {Value: "docker", Label: "Docker containers"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "  Name of the project.\nproject [notes]: \n" +
		"  How services run.\n  choices: process | docker\nruntime [process]: \n" +
		"warning: template input runtime must be one of: process, docker\n" +
		"  How services run.\n  choices: process | docker\nruntime [process]: \n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
	if !reflect.DeepEqual(result.Values, map[string]string{"project": "notes", "runtime": "docker"}) ||
		!reflect.DeepEqual(result.Origins, map[string]Origin{"project": OriginDefault, "runtime": OriginChanged}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestScriptedBoolSecretAndMultiselect(t *testing.T) {
	var output bytes.Buffer
	result, err := Run(context.Background(), Request{
		Mode: ModeScripted, In: strings.NewReader("yes\nno\nentered-secret\n[\"a\",\"b\"]\n\n"), Err: &output,
		Inputs: []api.TemplateInputDescriptor{
			{Name: "enabled", Question: true, Type: "bool", Default: "false"},
			{Name: "logging", Question: true, Type: "boolean", Default: "true"},
			{Name: "api_key", Question: true, Secret: true, Default: "hidden-default"},
			{Name: "features", Question: true, Multiselect: true, Choices: []api.TemplateInputChoice{{Value: "a"}, {Value: "b"}}},
			{Name: "empty", Question: true, Multiselect: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"enabled": "true", "logging": "false", "api_key": "entered-secret", "features": `["a","b"]`, "empty": "[]"}
	if !reflect.DeepEqual(result.Values, want) {
		t.Fatalf("values = %#v, want %#v", result.Values, want)
	}
	for _, prompt := range []string{"enabled [y/N]: ", "logging [Y/n]: ", "api_key: ", "features [[]]: "} {
		if !strings.Contains(output.String(), prompt) {
			t.Errorf("missing prompt %q: %q", prompt, output.String())
		}
	}
	for _, secret := range []string{"hidden-default", "entered-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("secret %q leaked: %q", secret, output.String())
		}
	}
}

func TestScriptedSkipsProvidedAndReadOnly(t *testing.T) {
	var output bytes.Buffer
	result, err := Run(context.Background(), Request{
		Mode: ModeScripted, In: failReader{t: t}, Err: &output,
		Provided: map[string]string{"project": "notes", "extra": "keep"}, Origins: map[string]Origin{"project": OriginAnswers},
		Inputs: []api.TemplateInputDescriptor{
			{Name: "project", Question: true, Default: "app"},
			{Name: "fixed", Question: true, Immutable: true, Default: "fixed"},
			{Name: "generated", Question: true, Generated: true, Required: true},
			{Name: "metadata"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("unexpected prompts: %q", output.String())
	}
	if result.Values["project"] != "notes" || result.Values["fixed"] != "fixed" || result.Values["extra"] != "keep" || result.Origins["project"] != OriginAnswers {
		t.Fatalf("result = %#v", result)
	}
}

func TestScriptedErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    api.TemplateInputDescriptor
		stdin    string
		want     string
		warnings int
	}{
		{name: "EOF", input: api.TemplateInputDescriptor{Name: "topic", Question: true, Required: true},
			want: "template input topic requires interactive input; use --yes to accept defaults or --input topic=value"},
		{name: "invalid integer retry limit", input: api.TemplateInputDescriptor{Name: "port", Question: true, Type: "int"},
			stdin: "a\nb\nc\n42\n", want: "template input port must be an integer", warnings: 3},
		{name: "required retry limit", input: api.TemplateInputDescriptor{Name: "topic", Question: true, Required: true},
			stdin: "\n\n\n", want: "template input topic is required; pass --input topic=value", warnings: 3},
		{name: "invalid choice retry limit", input: api.TemplateInputDescriptor{Name: "runtime", Question: true, Choices: []api.TemplateInputChoice{{Value: "process"}, {Value: "docker"}}},
			stdin: "a\nb\nc\ndocker\n", want: "template input runtime must be one of: process, docker", warnings: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := Run(context.Background(), Request{Mode: ModeScripted, In: strings.NewReader(tc.stdin), Err: &output, Inputs: []api.TemplateInputDescriptor{tc.input}})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if got := strings.Count(output.String(), "warning: "); got != tc.warnings {
				t.Fatalf("warnings = %d, want %d: %q", got, tc.warnings, output.String())
			}
		})
	}
}

func TestScriptedAcceptsFinalLineWithoutNewline(t *testing.T) {
	result, err := Run(context.Background(), Request{
		Mode: ModeScripted, In: strings.NewReader("notes"),
		Inputs: []api.TemplateInputDescriptor{{Name: "project", Question: true, Required: true}},
	})
	if err != nil || result.Values["project"] != "notes" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestWrappedHelp(t *testing.T) {
	help := strings.Repeat("A useful description with unicode café. ", 8)
	for _, indent := range []int{2, 6} {
		output := WrappedHelp(help, indent)
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if !strings.HasPrefix(line, strings.Repeat(" ", indent)) || utf8.RuneCountInString(line) > 78 {
				t.Errorf("invalid wrapped help line for indent %d: %q", indent, line)
			}
		}
		if strings.Join(strings.Fields(output), " ") != strings.TrimSpace(help) {
			t.Errorf("wrapped text changed: %q", output)
		}
	}
	if got := WrappedHelp("first\n\nlast", 2); got != "  first\n  \n  last\n" {
		t.Errorf("paragraphs changed: %q", got)
	}
}
