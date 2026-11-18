package copierx

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A recorded list that holds a mapping, at any depth, is a structured answer:
// it reads as the JSON value it records, with YAML's own booleans, numbers
// and nulls, an alias as what it names, and other scalars as their text.
func TestStructuredAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
		structured       bool
		err              string
	}{
		{name: "a list of mappings", yaml: "- name: app\n  ref: \"\"\n  ssh: true\n- name: lib\n  ssh: false\n", structured: true, want: `[{"name":"app","ref":"","ssh":true},{"name":"lib","ssh":false}]`},
		{name: "a mapping in a nested list", yaml: "[[{b: 1, a: [x, 2.5, null, \"3\", 2026-09-05]}], [plain]]\n", structured: true, want: `[[{"a":["x",2.5,null,"3","2026-09-05"],"b":1}],["plain"]]`},
		{name: "an alias", yaml: "- &one {k: v}\n- *one\n", structured: true, want: `[{"k":"v"},{"k":"v"}]`},
		{name: "a list of scalars and lists", yaml: "[a, [b, 1], true]\n"},
		{name: "an empty list", yaml: "[]\n"},
		{name: "a key that is no scalar", yaml: "- {[a, b]: v}\n", structured: true, err: "holds a mapping whose keys are not plain scalars"},
		{name: "a merge key", yaml: "- &base {k: v}\n- {<<: *base}\n", structured: true, err: "holds a mapping whose keys are not plain scalars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document yaml.Node
			if err := yaml.Unmarshal([]byte(tc.yaml), &document); err != nil {
				t.Fatal(err)
			}
			node := document.Content[0]
			if got := AnswerHoldsMapping(node); got != tc.structured {
				t.Fatalf("AnswerHoldsMapping() = %v, want %v", got, tc.structured)
			}
			if !tc.structured {
				return
			}
			value, err := StructuredAnswer(node)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("StructuredAnswer() error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("StructuredAnswer() error = %v", err)
			}
			encoded, err := json.Marshal(value)
			if err != nil || string(encoded) != tc.want {
				t.Fatalf("StructuredAnswer() = %s, %v; want %s", encoded, err, tc.want)
			}
		})
	}
}
