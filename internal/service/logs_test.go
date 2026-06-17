package service

import "testing"

func TestInferLogLevel(t *testing.T) {
	cases := []struct {
		line string
		want string // "" means nil (unknown)
	}{
		{`level=warn msg="disk low"`, "WARN"},
		{`level=warning msg=x`, "WARN"},
		{`time=... level=error err=boom`, "ERROR"},
		{`[INFO] server started`, "INFO"},
		{`[DEBUG] cache miss`, "DEBUG"},
		{`level=fatal`, "ERROR"},
		{`level=panic`, "ERROR"},
		{`level=err`, "ERROR"},
		{`GET /health 200 OK`, ""},
		{`no error occurred during startup`, ""}, // mid-sentence word, not a marker
		{`information about the run`, ""},        // must not match "info" inside a word
	}
	for _, c := range cases {
		got := inferLogLevel(c.line)
		if c.want == "" {
			if got != nil {
				t.Errorf("inferLogLevel(%q) = %q, want nil", c.line, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("inferLogLevel(%q) = nil, want %q", c.line, c.want)
			continue
		}
		if *got != c.want {
			t.Errorf("inferLogLevel(%q) = %q, want %q", c.line, *got, c.want)
		}
	}
}
