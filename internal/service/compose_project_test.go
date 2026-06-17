package service

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// composeProjectRe is Docker Compose's documented project-name constraint.
var composeProjectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func TestComposeProjectName_UniquePerRoot(t *testing.T) {
	a := composeProjectName("notes-angee", "/srv/workspaces/one/.angee")
	b := composeProjectName("notes-angee", "/srv/workspaces/two/.angee")
	if a == b {
		t.Fatalf("same name + different root produced identical project %q", a)
	}
	for _, name := range []string{a, b} {
		if !composeProjectRe.MatchString(name) {
			t.Errorf("project %q does not match %s", name, composeProjectRe)
		}
	}
}

func TestComposeProjectName_StablePerRoot(t *testing.T) {
	const name, root = "notes-angee", "/srv/workspaces/one/.angee"
	if got, want := composeProjectName(name, root), composeProjectName(name, root); got != want {
		t.Fatalf("composeProjectName not stable: %q != %q", got, want)
	}
}

func TestComposeProjectName_Sanitizes(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantBase string // the part before the "-<8 hex>" suffix
	}{
		{"plain", "notes-angee", "notes-angee"},
		{"upper", "Notes-Angee", "notes-angee"},
		{"spaces_and_symbols", "My Cool Stack!!", "my-cool-stack"},
		{"leading_illegal", "__private", "private"},
		{"collapse_runs", "a   b///c", "a-b-c"},
		{"keep_underscore", "snake_case", "snake_case"},
		{"only_illegal", "***", "angee"}, // empty base falls back
		{"empty", "", "angee"},
		{"unicode", "café", "caf"},
	}
	suffix := regexp.MustCompile(`-[0-9a-f]{8}$`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeProjectName(tc.input, "/some/root")
			if !composeProjectRe.MatchString(got) {
				t.Errorf("project %q does not match %s", got, composeProjectRe)
			}
			if !suffix.MatchString(got) {
				t.Errorf("project %q missing 8-hex root suffix", got)
			}
			base := suffix.ReplaceAllString(got, "")
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q (full %q)", base, tc.wantBase, got)
			}
		})
	}
}

// FuzzComposeProjectName locks the load-bearing invariant: whatever the
// manifest name, the derived project name must always satisfy Docker Compose's
// project-name regex. This guards against a future "optimization" of the
// sanitize/trim logic silently producing invalid names.
func FuzzComposeProjectName(f *testing.F) {
	for _, seed := range []string{
		"", "notes-angee", "Notes Angee!!", "__private", "***", "café",
		"a   b///c", "snake_case", strings.Repeat("a", 300),
	} {
		f.Add(seed, "/srv/root")
	}
	f.Fuzz(func(t *testing.T, name, root string) {
		got := composeProjectName(name, root)
		if !composeProjectRe.MatchString(got) {
			t.Errorf("composeProjectName(%q, %q) = %q, not a valid Compose project name", name, root, got)
		}
	})
}

// TestCompile_SameNameDistinctProjects is the regression for the collision the
// proposal fixes: two stacks with identical name: but different roots must
// compile to disjoint Compose projects, while still wiring the edge network
// consistently within each project (the path that originally broke with a 1006).
func TestCompile_SameNameDistinctProjects(t *testing.T) {
	build := func() *manifest.Stack {
		s := &manifest.Stack{
			Name:    "notes-angee",
			Ingress: manifest.Ingress{Type: "caddy", Domain: "agents.localhost"},
			Services: map[string]manifest.Service{
				"agent": {
					Runtime: manifest.RuntimeContainer,
					Image:   "nginx:latest",
					Route:   &manifest.Route{Port: 3008},
				},
			},
		}
		s.Defaults()
		if err := s.Validate(); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
		return s
	}

	c1, err := Compile(build(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Compile() one error = %v", err)
	}
	c2, err := Compile(build(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Compile() two error = %v", err)
	}

	if c1.Compose.Name == c2.Compose.Name {
		t.Fatalf("identical name + distinct roots shared project %q", c1.Compose.Name)
	}

	// Within each project the edge service must attach to a network that the
	// compose file actually declares; Docker re-namespaces that key under the
	// (now-distinct) project, so the two stacks no longer collide.
	for i, c := range []*CompiledStack{c1, c2} {
		edge, ok := c.Compose.Services["edge"]
		if !ok {
			t.Fatalf("stack %d: edge service missing", i)
		}
		for _, net := range edge.Networks {
			if _, ok := c.Compose.Networks[net]; !ok {
				t.Errorf("stack %d: edge references undeclared network %q", i, net)
			}
		}
	}
}
