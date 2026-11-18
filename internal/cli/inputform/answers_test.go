package inputform

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAnswersFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want map[string]string
		err  string
	}{
		{
			name: "verbatim scalars",
			yaml: "created: 2026-09-05\nwhen: 2026-09-05T12:30:00Z\nbig: 100000000000000000000\nratio: 1.0\nhex: 0x1F\nyes_word: yes\n",
			want: map[string]string{"created": "2026-09-05", "when": "2026-09-05T12:30:00Z", "big": "100000000000000000000", "ratio": "1.0", "hex": "0x1F", "yes_word": "yes"},
		},
		{
			name: "aliases and merge keys",
			yaml: "name: &n notes\nalias: *n\nlist: [*n, b]\n",
			want: map[string]string{"name": "notes", "alias": "notes", "list": "[\"notes\",\"b\"]"},
		},
		{
			name: "Copier answers",
			yaml: "_src_path: /templates/stacks/dev\n_commit: abc123\n_angee:\n  metadata: ignored\nproject: notes\n",
			want: map[string]string{"project": "notes"},
		},
		{
			name: "scalars",
			yaml: "enabled: true\nlogging: false\nport: 8080\nratio: 1.5\nquoted: '001'\nempty: ''\nnull_value: null\n",
			want: map[string]string{"enabled": "true", "logging": "false", "port": "8080", "ratio": "1.5", "quoted": "001", "empty": "", "null_value": ""},
		},
		{
			name: "lists encode scalar strings for multiselect",
			yaml: "features: [a, b]\nnumbers: [1, 2]\nbools: [true, false]\nempty: []\n",
			want: map[string]string{"features": `["a","b"]`, "numbers": `["1","2"]`, "bools": `["true","false"]`, "empty": `[]`},
		},
		{name: "nested lists preserve JSON structure", yaml: "nested: [[a, b], [1]]\n", want: map[string]string{"nested": `[["a","b"],["1"]]`}},
		{name: "mapping", yaml: "config:\n  key: value\n", err: "key config is a mapping; only scalars and lists are supported"},
		{
			// Copier records a `type: json` answer, such as a repository list,
			// as a list of mappings: it is passed on as the JSON it was, with
			// its booleans, numbers and nulls, and keys sorted.
			name: "a list that holds a mapping is a structured answer",
			yaml: "repositories:\n- name: app\n  ref: \"\"\n  repo: git@example.com:acme/app.git\n  ssh: true\nnested: [[{b: 1, a: [x, 2.5, null, false], c: \"3\"}], [plain]]\n",
			want: map[string]string{
				"repositories": `[{"name":"app","ref":"","repo":"git@example.com:acme/app.git","ssh":true}]`,
				"nested":       `[[{"a":["x",2.5,null,false],"b":1,"c":"3"}],["plain"]]`,
			},
		},
		{name: "mapping in a mapping in a list", yaml: "config: [{key: {inner: value}}]\n", want: map[string]string{"config": `[{"key":{"inner":"value"}}]`}},
		{name: "mapping with a non-scalar key in a list", yaml: "config: [{[a]: value}]\n", err: "key config holds a mapping whose keys are not plain scalars"},
		{name: "a number JSON cannot hold, in a structured answer", yaml: "config: [{ratio: .inf}]\n", err: "key config json: unsupported value: +Inf"},
		{name: "sequence document", yaml: "- a\n- b\n", err: "expected a YAML mapping"},
		{name: "scalar document", yaml: "hello\n", err: "expected a YAML mapping"},
		{name: "null document", yaml: "null\n", err: "expected a YAML mapping"},
		{name: "empty", want: map[string]string{}},
		{name: "comments only", yaml: "# no answers yet\n", want: map[string]string{}},
		{name: "empty mapping", yaml: "{}\n", want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".copier-answers.stack.yml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadAnswersFile(path)
			if tc.err != "" {
				wantErr := "answers file " + path + ": " + tc.err
				if err == nil || err.Error() != wantErr {
					t.Fatalf("LoadAnswersFile error = %v, want %q", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("LoadAnswersFile = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLoadAnswersFileReadAndParseErrorsNameFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "answers.yml")
	_, err := LoadAnswersFile(path)
	if err == nil || !strings.HasPrefix(err.Error(), "answers file "+path+": ") {
		t.Fatalf("read error = %v", err)
	}
	if err := os.WriteFile(path, []byte("project: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadAnswersFile(path)
	if err == nil || !strings.HasPrefix(err.Error(), "answers file "+path+": ") {
		t.Fatalf("parse error = %v", err)
	}
}
