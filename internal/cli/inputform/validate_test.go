package inputform

import (
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

func TestValidate(t *testing.T) {
	choices := []api.TemplateInputChoice{{Value: "a", Label: "First"}, {Value: "b", Label: "Second"}}
	for _, tc := range []struct {
		name        string
		typ, value  string
		choices     []api.TemplateInputChoice
		multiselect bool
		required    bool
		want        string
	}{
		{name: "string", typ: "str", value: "notes"},
		{name: "empty optional", typ: "str"},
		{name: "path", typ: "path", value: "/a/path"},
		{name: "unknown type", typ: "custom", value: "anything"},
		{name: "int", typ: "int", value: "-23"},
		{name: "integer alias", typ: "integer", value: "42"},
		{name: "invalid int", typ: "int", value: "1.5", want: "template input answer must be an integer"},
		{name: "empty int", typ: "int", want: "template input answer must be an integer"},
		{name: "true", typ: "bool", value: "true"},
		{name: "false", typ: "boolean", value: "false"},
		{name: "one", typ: "bool", value: "1"},
		{name: "invalid bool", typ: "bool", value: "maybe", want: "template input answer must be a boolean"},
		{name: "choice value", choices: choices, value: "b"},
		{name: "choice label rejected", choices: choices, value: "Second", want: "template input answer must be one of: a, b"},
		{name: "empty choice rejected", choices: choices, want: "template input answer must be one of: a, b"},
		{name: "required", required: true, want: "template input answer is required; pass --input answer=value"},
		{name: "multiselect", multiselect: true, choices: choices, value: `["a","b"]`},
		{name: "multiselect empty", multiselect: true, choices: choices, value: `[]`},
		{name: "multiselect free values", multiselect: true, value: `["one,two","three"]`},
		{name: "multiselect wrong choice", multiselect: true, choices: choices, value: `["c"]`, want: "template input answer must be one of: a, b"},
		{name: "multiselect comma list", multiselect: true, value: "a,b", want: "template input answer must be a JSON array of strings"},
		{name: "multiselect scalar", multiselect: true, value: `"a"`, want: "template input answer must be a JSON array of strings"},
		{name: "multiselect number", multiselect: true, value: `[1]`, want: "template input answer must be a JSON array of strings"},
		{name: "multiselect null", multiselect: true, value: `null`, want: "template input answer must be a JSON array of strings"},
		{name: "multiselect null element", multiselect: true, value: `[null]`, want: "template input answer must be a JSON array of strings"},
		{name: "multiselect required", multiselect: true, required: true, value: `[]`, want: "template input answer is required; pass --input answer=value"},
		{name: "multiselect int strings", multiselect: true, typ: "int", value: `["1","2"]`},
		{name: "multiselect bad int", multiselect: true, typ: "int", value: `["one"]`, want: "template input answer must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := api.TemplateInputDescriptor{
				Name: "answer", Type: tc.typ, Choices: tc.choices,
				Multiselect: tc.multiselect, Required: tc.required,
			}
			err := Validate(desc, tc.value)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
