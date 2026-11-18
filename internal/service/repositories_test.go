package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// Each accepted spelling of a repository gives its name (the last element of
// its path, without .git), its ref (what follows the first @ of the path),
// the URL it is cloned from, and whether that URL is reached over ssh.
func TestParseRepositorySpellings(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want repositoryEntry
	}{
		{"ang-ee/angee-arp", repositoryEntry{Name: "angee-arp", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"ang-ee/angee-arp@feature/arp-ap", repositoryEntry{Name: "angee-arp", Ref: "feature/arp-ap", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"ang-ee/angee-arp@v1.2.3", repositoryEntry{Name: "angee-arp", Ref: "v1.2.3", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"https://github.com/ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"https://gitlab.example.com/group/sub/angee-arp", repositoryEntry{Name: "angee-arp", Repo: "https://gitlab.example.com/group/sub/angee-arp"}},
		{"https://github.com/ang-ee/angee-arp.git@main", repositoryEntry{Name: "angee-arp", Ref: "main", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		// A scheme is recorded in lowercase.
		{"HTTPS://github.com/ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "https://github.com/ang-ee/angee-arp.git"}},
		{"SSH://git@github.com/ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "ssh://git@github.com/ang-ee/angee-arp.git", SSH: true}},
		{"git@github.com:ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "git@github.com:ang-ee/angee-arp.git", SSH: true}},
		// The @ of the user is the URL's; the ref follows the first @ of the path.
		{"git@github.com:ang-ee/angee-arp.git@feature/arp-ap", repositoryEntry{Name: "angee-arp", Ref: "feature/arp-ap", Repo: "git@github.com:ang-ee/angee-arp.git", SSH: true}},
		{"git@github.com:ang-ee/angee-arp.git@release@2", repositoryEntry{Name: "angee-arp", Ref: "release@2", Repo: "git@github.com:ang-ee/angee-arp.git", SSH: true}},
		{"deploy:angee-arp", repositoryEntry{Name: "angee-arp", Repo: "deploy:angee-arp", SSH: true}},
		{"ssh://git@github.com/ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "ssh://git@github.com/ang-ee/angee-arp.git", SSH: true}},
		{"ssh://git@git.example.com:2222/ang-ee/angee-arp.git@main", repositoryEntry{Name: "angee-arp", Ref: "main", Repo: "ssh://git@git.example.com:2222/ang-ee/angee-arp.git", SSH: true}},
		{"git+ssh://git@github.com/ang-ee/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "git+ssh://git@github.com/ang-ee/angee-arp.git", SSH: true}},
		{"/srv/git/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "/srv/git/angee-arp.git"}},
		{"/srv/git/angee-arp/@main", repositoryEntry{Name: "angee-arp", Ref: "main", Repo: "/srv/git/angee-arp/"}},
		{"file:///srv/git/angee-arp.git", repositoryEntry{Name: "angee-arp", Repo: "file:///srv/git/angee-arp.git"}},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			got, err := parseRepository(tc.spec)
			if err != nil {
				t.Fatalf("parseRepository(%q) error = %v", tc.spec, err)
			}
			if got != tc.want {
				t.Fatalf("parseRepository(%q) = %+v, want %+v", tc.spec, got, tc.want)
			}
			// The entry's own URL, with its ref, reads back as the entry.
			spec := got.Repo
			if got.Ref != "" {
				spec += "@" + got.Ref
			}
			if again, err := parseRepository(spec); err != nil || again != got {
				t.Fatalf("parseRepository(%q) = %+v, %v; want the same entry", spec, again, err)
			}
		})
	}
}

func TestParseRepositoryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, spec, want string
	}{
		{"empty", "", "a repository is empty"},
		{"a bare name", "angee-arp", `"angee-arp" is not a repository: give a URL`},
		{"three path elements", "a/b/c", `"a/b/c" is not a repository`},
		{"a relative path", "./angee-arp", `"./angee-arp" is not a repository`},
		{"a parent path", "../angee-arp", `"../angee-arp" is not a repository`},
		{"an uppercase name", "Ang-ee/Angee-ARP", `its name "Angee-ARP", the last element of its path without .git, must start with a lowercase letter or a digit`},
		{"a dotted name", "ang-ee/angee.arp", `its name "angee.arp"`},
		{"an underscore", "git@github.com:ang-ee/angee_arp.git", `its name "angee_arp"`},
		{"a name that is ..", "https://example.com/a/..", `its name ".."`},
		{"no path", "https://example.com", `has no path after its host`},
		{"a host with only a slash", "https://example.com/", `has no path after its host`},
		{"an option-shaped host", "-oProxyCommand=evil:angee-arp", `repository "-oProxyCommand=evil:angee-arp" starts with '-'`},
		{"an option-shaped path", "-/angee-arp", `starts with '-'`},
		{"a space", "ang-ee/angee arp", `contains a space or a control character`},
		{"a newline", "ang-ee/angee-arp\n", `contains a space or a control character`},
		{"a password", "https://bot:s3cr3t@github.com/ang-ee/angee-arp.git", `repository "https://***@github.com/ang-ee/angee-arp.git" carries a password in its URL`},
		{"a token as the user of a password", "https://s3cr3t:x-oauth-basic@github.com/ang-ee/angee-arp.git", `repository "https://***@github.com/ang-ee/angee-arp.git" carries a password in its URL`},
		{"a password over ssh", "ssh://git:s3cr3t@github.com/ang-ee/angee-arp.git", `repository "ssh://***@github.com/ang-ee/angee-arp.git" carries a password in its URL`},
		{"a password under an uppercase scheme", "HTTPS://bot:s3cr3t@github.com/ang-ee/Angee-arp.git", `repository "https://***@github.com/ang-ee/Angee-arp.git" carries a password in its URL`},
		{"a token as the user", "https://s3cr3t@github.com/ang-ee/angee-arp.git", `repository "https://***@github.com/ang-ee/angee-arp.git" carries a user in its URL, which over https is often a token`},
		{"a user over http, in a refusal of something else", "https://s3cr3t@github.com/ang-ee/Angee", `carries a user in its URL`},
		{"something like a URL", "1http://example.com/a/b", `"1http://example.com/a/b" is not a repository`},
		{"a query", "https://example.com/x/app.git?token=s3cr3t", `repository "https://example.com/x/app.git…" holds a '?' or a '#'`},
		{"a query and a user", "https://bot@example.com/x/app.git?token=s3cr3t", `repository "https://***@example.com/x/app.git…" carries a user in its URL`},
		{"a query after the host", "https://example.com?token=s3cr3t", `repository "https://example.com/" has no path after its host`},
		{"a fragment", "ssh://git@example.com/x/app.git#s3cr3t", `repository "ssh://git@example.com/x/app.git…" holds a '?' or a '#'`},
		{"a query in an ssh remote", "git@example.com:x/app.git?token=s3cr3t", `repository "git@example.com:x/app.git…" holds a '?' or a '#'`},
		{"a query and a bad name", "ang-ee/App?token=s3cr3t", `repository "ang-ee/App…" holds a '?' or a '#'`},
		{"a query and a space", "https://example.com/x/a pp.git?token=s3cr3t", `repository "https://example.com/x/a pp.git…" holds a '?' or a '#'`},
		{"an empty ref", "ang-ee/angee-arp@", `ref "" is empty: leave out the @ to take the default branch`},
		{"an option-shaped ref", "ang-ee/angee-arp@--upload-pack=x", `ref "--upload-pack=x" starts with '-' or '/'`},
		{"an absolute ref", "ang-ee/angee-arp@/main", `starts with '-' or '/'`},
		{"a ref with ..", "ang-ee/angee-arp@a..b", `holds '..', '//' or '@{', or is '@'`},
		{"a ref with //", "ang-ee/angee-arp@a//b", `holds '..', '//' or '@{', or is '@'`},
		{"a ref with a caret", "ang-ee/angee-arp@main^", `holds one of ~ ^ : ? * [ \`},
		{"a ref with a colon", "git@github.com:ang-ee/angee-arp.git@a:b", `holds one of ~ ^ : ? * [ \`},
		{"a ref that ends in /", "ang-ee/angee-arp@feature/", `ends with '/' or '.'`},
		{"a ref that ends in .", "ang-ee/angee-arp@main.", `ends with '/' or '.'`},
		{"a ref that ends in .lock", "ang-ee/angee-arp@main.lock", `has an element that starts with '.' or ends with '.lock'`},
		{"a ref with an element that ends in .lock", "ang-ee/angee-arp@a.lock/b", `has an element that starts with '.' or ends with '.lock'`},
		{"a ref with a hidden element", "ang-ee/angee-arp@feature/.x", `has an element that starts with '.' or ends with '.lock'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRepository(tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseRepository(%q) = %+v, %v; want an error with %q", tc.spec, got, err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("parseRepository(%q) error quotes the password: %v", tc.spec, err)
			}
		})
	}
}

// A list is a JSON array of strings and objects. Its canonical value is the
// entries sorted by name, each with the keys name, ref, repo and ssh in that
// order, and encoding a canonical value again yields the same bytes, as does
// an answer recorded by copier, whatever order it reads back in.
func TestRepositoryListCanonicalForm(t *testing.T) {
	const canonical = `[{"name":"angee-arp","ref":"","repo":"git@github.com:ang-ee/angee-arp.git","ssh":true},{"name":"angee-odoo","ref":"feature/arp-ap","repo":"https://github.com/ang-ee/angee-odoo.git","ssh":false}]`
	for _, tc := range []struct {
		name, raw string
	}{
		{"flag strings", `["ang-ee/angee-odoo@feature/arp-ap","git@github.com:ang-ee/angee-arp.git"]`},
		{"objects", `[{"repo":"ang-ee/angee-odoo","ref":"feature/arp-ap"},{"repo":"git@github.com:ang-ee/angee-arp.git"}]`},
		{"strings and objects", `[{"repo":"git@github.com:ang-ee/angee-arp.git","ref":null}, "ang-ee/angee-odoo@feature/arp-ap"]`},
		{"an object whose repo carries its ref", `[{"repo":"ang-ee/angee-odoo@feature/arp-ap","ref":"feature/arp-ap"},"git@github.com:ang-ee/angee-arp.git"]`},
		{"the canonical value", canonical},
		{"a recorded answer, its keys in another order", `[{"ssh":true,"repo":"git@github.com:ang-ee/angee-arp.git","ref":null,"name":"angee-arp"},{"repo":"https://github.com/ang-ee/angee-odoo.git","name":"angee-odoo","ssh":false,"ref":"feature/arp-ap"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := parseRepositoryList(RepositoriesInput, tc.raw)
			if err != nil {
				t.Fatalf("parseRepositoryList() error = %v", err)
			}
			if got := encodeRepositoryList(entries); got != canonical {
				t.Fatalf("canonical value =\n%s\nwant\n%s", got, canonical)
			}
		})
	}
	for _, raw := range []string{"", "  ", "[]", "null"} {
		entries, err := parseRepositoryList(RepositoriesInput, raw)
		if err != nil || encodeRepositoryList(entries) != "[]" {
			t.Fatalf("parseRepositoryList(%q) = %v, %v; want the empty list", raw, entries, err)
		}
	}
	// A URL is recorded as written, not with its & escaped.
	entries, err := parseRepositoryList(RepositoriesInput, `["https://example.com/a&b/addons"]`)
	if err != nil || encodeRepositoryList(entries) != `[{"name":"addons","ref":"","repo":"https://example.com/a&b/addons","ssh":false}]` {
		t.Fatalf("parseRepositoryList(&) = %v, %v; want the URL as written", entries, err)
	}
}

func TestRepositoryListRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"not JSON", `ang-ee/angee-arp`, `repositories: must be a JSON array of repositories`},
		{"an object, not an array", `{"repo":"ang-ee/angee-arp"}`, `repositories: must be a JSON array of repositories`},
		{"a number", `[1]`, `repositories: entry 1: must be a "<repo>[@<ref>]" string or an object`},
		{"an object without a repo", `[{"ref":"main"}]`, `entry 1: an object needs a repo`},
		{"an unknown key", `[{"repo":"ang-ee/angee-arp","distribution":"arp"}]`, `entry 1: has an unknown key "distribution": an entry takes repo and ref`},
		{"a chosen name", `[{"repo":"ang-ee/angee-arp","name":"arp"}]`, `entry 1: the repository is named "angee-arp" by its path, not "arp"`},
		{"an ssh that its URL denies", `[{"repo":"ang-ee/angee-arp","ssh":true}]`, `entry 1: repository "angee-arp": ssh is false by its URL, not true`},
		{"two refs", `[{"repo":"ang-ee/angee-arp@main","ref":"next"}]`, `entry 1: repository "angee-arp" names ref "main" in its repo and "next" as its ref`},
		{"a bad ref in an object", `[{"repo":"ang-ee/angee-arp","ref":"-x"}]`, `entry 1: repository "angee-arp": ref "-x" starts with '-' or '/'`},
		{"a ref with a space in an object", `[{"repo":"ang-ee/angee-arp","ref":"main --upload-pack=x"}]`, `entry 1: repository "angee-arp": ref "main --upload-pack=x" holds a space or a control character`},
		{"a ref with a newline in an object", `[{"repo":"ang-ee/angee-arp","ref":"main\nx"}]`, `holds a space or a control character`},
		{"an ssh that is text", `[{"repo":"git@github.com:ang-ee/angee-arp.git","ssh":"true"}]`, `entry 1: ssh must be true or false`},
		{"a repository twice", `["ang-ee/angee-arp","git@github.com:fork/angee-arp.git"]`, `repositories: lists repository "angee-arp" twice`},
		{"the second entry", `["ang-ee/angee-arp","nope"]`, `entry 2: "nope" is not a repository`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := parseRepositoryList(RepositoriesInput, tc.raw)
			var invalid *InvalidInputError
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.As(err, &invalid) || invalid.Field != RepositoriesInput {
				t.Fatalf("parseRepositoryList(%s) = %v, %v; want an invalid %s input with %q", tc.raw, entries, err, RepositoriesInput, tc.want)
			}
		})
	}
}

// repositoryStackCopier is a stack template's copier.yml that declares both
// repository list inputs as `type: json` questions, as a template that loops
// over them does.
const repositoryStackCopier = `_subdirectory: template
_templates_suffix: .jinja
_answers_file: .copier-answers.stack.yml
_angee:
  kind: stack
  name: dev
repositories:
  type: json
  default: "[]"
extra_sources:
  type: json
  default: "[]"
`

// repositoryStackManifest renders one source per entry of each list: a
// repository's and an extra source's alike, each marked for a slot, and with
// a key of its own when it is reached over ssh.
const repositoryStackManifest = `version: 1
kind: stack
name: demo
template:
  active: stacks/dev
  answers_file: .copier-answers.stack.yml
sources:
  framework:
    kind: git
    repo: https://example.invalid/framework.git
{% for repository in repositories %}  {{ repository.name }}:
    kind: git
    repo: {{ repository.repo|to_json }}
    default_ref: {{ repository.ref|default:"main"|to_json }}
    cache_path: sources/{{ repository.name }}
    slot: true
{% if repository.ssh %}    auth:
      ssh_key_secret: deploy-key-{{ repository.name }}
{% endif %}{% endfor %}{% for repository in extra_sources %}  {{ repository.name }}:
    kind: git
    repo: {{ repository.repo|to_json }}
    default_ref: {{ repository.ref|default:"main"|to_json }}
    slot: true
{% endfor %}{% if repositories or extra_sources %}secrets:
{% for repository in repositories %}{% if repository.ssh %}  deploy-key-{{ repository.name }}:
    required: true
{% endif %}{% endfor %}{% endif %}services:
  web:
    runtime: container
    image: nginx:latest
`

// writeRepositoryStackTemplate writes stack template dev under project with
// the given copier.yml and manifest body, and an ENTRIES file that renders
// how many repositories the template saw.
func writeRepositoryStackTemplate(t *testing.T, project, copierYML, body string) string {
	t.Helper()
	dir := filepath.Join(project, ".templates", "stacks", "dev")
	mustMkdirAll(t, filepath.Join(dir, "template"))
	mustWriteFile(t, filepath.Join(dir, "copier.yml"), copierYML)
	mustWriteFile(t, filepath.Join(dir, "template", "angee.yaml.jinja"), body)
	mustWriteFile(t, filepath.Join(dir, "template", "ENTRIES.jinja"), "{{ repositories|length }} repositories, {{ extra_sources|length }} extra sources\n")
	return dir
}

// The lists reach the template through the real engine: copier casts the
// canonical JSON string of a `type: json` question, the template loops over
// its entries, reads their fields and tests the boolean, and the stack that
// comes out has one source per entry.
func TestStackInitRendersRepositoryLists(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeRepositoryStackTemplate(t, project, repositoryStackCopier, repositoryStackManifest)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	inputs := map[string]string{
		// As `--repository` packs them, and as an object.
		RepositoriesInput: `["git@github.com:phro-gg/angee-omelas.git", {"repo": "ang-ee/angee-odoo", "ref": "feature/arp-ap"}]`,
		ExtraSourcesInput: `["git@github.com:ang-ee/angee-arp.git@v2"]`,
	}
	initialized, err := p.StackInit(ctx, "dev", "stack", inputs, false)
	if err != nil {
		t.Fatalf("StackInit() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(initialized.Root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	want := map[string]manifest.Source{
		"framework":    {Kind: "git", Repo: "https://example.invalid/framework.git"},
		"angee-omelas": {Kind: "git", Repo: "git@github.com:phro-gg/angee-omelas.git", DefaultRef: "main", CachePath: "sources/angee-omelas", Slot: true, Auth: manifest.SourceAuth{SSHKeySecret: "deploy-key-angee-omelas"}},
		"angee-odoo":   {Kind: "git", Repo: "https://github.com/ang-ee/angee-odoo.git", DefaultRef: "feature/arp-ap", CachePath: "sources/angee-odoo", Slot: true},
		"angee-arp":    {Kind: "git", Repo: "git@github.com:ang-ee/angee-arp.git", DefaultRef: "v2", Slot: true},
	}
	if len(stack.Sources) != len(want) {
		t.Fatalf("rendered sources = %v, want %v", sortedKeys(stack.Sources), sortedKeys(want))
	}
	for name, source := range want {
		if got := stack.Sources[name]; !yamlEqual(got, source) {
			t.Fatalf("rendered source %s = %+v, want %+v", name, got, source)
		}
	}
	if _, ok := stack.Secrets["deploy-key-angee-omelas"]; !ok || len(stack.Secrets) != 1 {
		t.Fatalf("rendered secrets = %v, want the key of the one ssh repository", sortedKeys(stack.Secrets))
	}
	entries, err := os.ReadFile(filepath.Join(initialized.Root, "ENTRIES"))
	if err != nil || string(entries) != "2 repositories, 1 extra sources\n" {
		t.Fatalf("ENTRIES = %q, %v; want the template to count entries, not characters", entries, err)
	}

	// copier records each list as the YAML of its entries, which reads back
	// as the canonical value it was rendered from.
	const repositories = `[{"name":"angee-odoo","ref":"feature/arp-ap","repo":"https://github.com/ang-ee/angee-odoo.git","ssh":false},{"name":"angee-omelas","ref":"","repo":"git@github.com:phro-gg/angee-omelas.git","ssh":true}]`
	const extraSources = `[{"name":"angee-arp","ref":"v2","repo":"git@github.com:ang-ee/angee-arp.git","ssh":true}]`
	recorded, err := os.ReadFile(filepath.Join(initialized.Root, ".copier-answers.stack.yml"))
	if err != nil || !strings.Contains(string(recorded), "- name: angee-odoo\n") || !strings.Contains(string(recorded), "ssh: true\n") {
		t.Fatalf("answers file = %q, %v; want the entries as YAML", recorded, err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root) error = %v", err)
	}
	described, err := stackPlatform.StackTemplateInputs(ctx)
	if err != nil {
		t.Fatalf("StackTemplateInputs() error = %v", err)
	}
	if described.Recorded[RepositoriesInput] != repositories || described.Recorded[ExtraSourcesInput] != extraSources {
		t.Fatalf("recorded answers = %v, want the canonical lists", described.Recorded)
	}

	// An update compiles the stack, which needs its required secrets.
	if _, err := stackPlatform.SecretSet(ctx, "deploy-key-angee-omelas", "not read here"); err != nil {
		t.Fatalf("SecretSet() error = %v", err)
	}
	// Replaying the recorded answers changes nothing, twice over, and so
	// does giving the recorded list again in another spelling.
	for _, given := range []map[string]string{nil, nil, {RepositoriesInput: `["ang-ee/angee-odoo@feature/arp-ap","git@github.com:phro-gg/angee-omelas.git"]`}} {
		result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: given})
		if err != nil {
			t.Fatalf("StackUpdateFromTemplate(%v) error = %v", given, err)
		}
		if result.Changed {
			t.Fatalf("StackUpdateFromTemplate(%v) changes = %v, want none", given, result.Changes)
		}
		after, err := os.ReadFile(filepath.Join(initialized.Root, ".copier-answers.stack.yml"))
		if err != nil || string(after) != string(recorded) {
			t.Fatalf("answers file after a replay =\n%s\nwant it unchanged:\n%s", after, recorded)
		}
	}

	// A list given on an update replaces the recorded one. A source the
	// template no longer renders is kept, as any the manifest holds alone.
	result, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{RepositoriesInput: `["ang-ee/angee-bridges"]`}})
	if err != nil || !result.Changed {
		t.Fatalf("StackUpdateFromTemplate(new list) = %+v, %v; want a change", result, err)
	}
	updated, err := stackPlatform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack() error = %v", err)
	}
	if got := updated.Sources["angee-bridges"]; got.Repo != "https://github.com/ang-ee/angee-bridges.git" || !got.Slot {
		t.Fatalf("source angee-bridges = %+v, want the new repository", got)
	}
	if _, kept := updated.Sources["angee-omelas"]; !kept {
		t.Fatalf("sources = %v, want the dropped repository's source kept", sortedKeys(updated.Sources))
	}
	described, err = stackPlatform.StackTemplateInputs(ctx)
	if err != nil || described.Recorded[RepositoriesInput] != `[{"name":"angee-bridges","ref":"","repo":"https://github.com/ang-ee/angee-bridges.git","ssh":false}]` || described.Recorded[ExtraSourcesInput] != extraSources {
		t.Fatalf("recorded answers after the update = %v, %v", described.Recorded, err)
	}

	// A repository given now in one list and recorded in the other is refused.
	_, err = stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{RepositoriesInput: `["ang-ee/angee-arp"]`}})
	if err == nil || !strings.Contains(err.Error(), `repository "angee-arp" is in both repositories and extra_sources`) {
		t.Fatalf("StackUpdateFromTemplate(in both lists) error = %v", err)
	}

	// The empty list replaces a recorded one too.
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{RepositoriesInput: `[]`}}); err != nil {
		t.Fatalf("StackUpdateFromTemplate(empty list) error = %v", err)
	}
	described, err = stackPlatform.StackTemplateInputs(ctx)
	if err != nil || described.Recorded[RepositoriesInput] != "[]" || described.Recorded[ExtraSourcesInput] != extraSources {
		t.Fatalf("recorded answers after the empty list = %v, %v", described.Recorded, err)
	}
}

// Without repositories the template renders none: the default, "[]", is the
// empty list, not a string of two characters.
func TestStackInitRendersNoRepositoriesByDefault(t *testing.T) {
	project := t.TempDir()
	writeRepositoryStackTemplate(t, project, repositoryStackCopier, repositoryStackManifest)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	initialized, err := p.StackInit(context.Background(), "dev", "stack", nil, false)
	if err != nil {
		t.Fatalf("StackInit() error = %v", err)
	}
	stack, err := manifest.LoadFile(manifest.Path(initialized.Root))
	if err != nil || len(stack.Sources) != 1 || len(stack.Secrets) != 0 {
		t.Fatalf("rendered stack sources = %v, secrets = %v, %v; want the template's own source alone", sortedKeys(stack.Sources), sortedKeys(stack.Secrets), err)
	}
	entries, err := os.ReadFile(filepath.Join(initialized.Root, "ENTRIES"))
	if err != nil || string(entries) != "0 repositories, 0 extra sources\n" {
		t.Fatalf("ENTRIES = %q, %v", entries, err)
	}
}

func TestStackInitRefusesRepositoryLists(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		copierYML string
		inputs    map[string]string
		field     string
		want      string
	}{
		{
			name:      "a list the template does not declare",
			copierYML: strings.Replace(repositoryStackCopier, "extra_sources:\n  type: json\n  default: \"[]\"\n", "", 1),
			inputs:    map[string]string{ExtraSourcesInput: `["ang-ee/angee-arp"]`},
			field:     ExtraSourcesInput,
			want:      `extra_sources: stack template "dev" declares no input "extra_sources", so it takes no --extra-source`,
		},
		{
			name:      "repositories the template does not declare",
			copierYML: "_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: stack\n  name: dev\n",
			inputs:    map[string]string{RepositoriesInput: `["ang-ee/angee-arp"]`},
			field:     RepositoriesInput,
			want:      `repositories: stack template "dev" declares no input "repositories", so it takes no --repository`,
		},
		{
			name:      "a repository in both lists",
			copierYML: repositoryStackCopier,
			inputs:    map[string]string{RepositoriesInput: `["ang-ee/angee-arp"]`, ExtraSourcesInput: `["git@github.com:ang-ee/angee-arp.git"]`},
			field:     ExtraSourcesInput,
			want:      `extra_sources: repository "angee-arp" is in both repositories and extra_sources: a stack has one source of a name`,
		},
		{
			name:      "an invalid entry",
			copierYML: repositoryStackCopier,
			inputs:    map[string]string{RepositoriesInput: `["Ang-ee/Angee"]`},
			field:     RepositoriesInput,
			want:      `repositories: entry 1: repository "Ang-ee/Angee": its name "Angee"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := t.TempDir()
			writeRepositoryStackTemplate(t, project, tc.copierYML, oneServiceTemplate)
			p, err := New(project)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, err = p.StackInit(ctx, "dev", "stack", tc.inputs, false)
			var invalid *InvalidInputError
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.As(err, &invalid) || invalid.Field != tc.field {
				t.Fatalf("StackInit() error = %v, want an invalid %s input with %q", err, tc.field, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(project, "stack")); !os.IsNotExist(statErr) {
				t.Fatalf("stack root after a refused init: %v, want nothing rendered", statErr)
			}
		})
	}
}

// An empty list asks for nothing, so a template that declares no such input
// takes it: an answers file recorded under another template replays.
func TestStackInitTakesAnEmptyListItDoesNotDeclare(t *testing.T) {
	project := t.TempDir()
	writeRepositoryStackTemplate(t, project, "_subdirectory: template\n_templates_suffix: .jinja\n_angee:\n  kind: stack\n  name: dev\n", oneServiceTemplate)
	if err := os.Remove(filepath.Join(project, ".templates", "stacks", "dev", "template", "ENTRIES.jinja")); err != nil {
		t.Fatal(err)
	}
	p, err := New(project)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := p.StackInit(context.Background(), "dev", "stack", map[string]string{RepositoriesInput: "[]", ExtraSourcesInput: ""}, false); err != nil {
		t.Fatalf("StackInit() error = %v", err)
	}
}

// A recorded list the template no longer declares is replayed as any other
// answer it no longer asks for, and refuses no update.
func TestStackUpdateKeepsARecordedListTheTemplateNoLongerDeclares(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	dir := writeRepositoryStackTemplate(t, project, repositoryStackCopier, repositoryStackManifest)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "stack", map[string]string{RepositoriesInput: `["ang-ee/angee-arp"]`}, false)
	if err != nil {
		t.Fatalf("StackInit() error = %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, "copier.yml"), "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.stack.yml\n_angee:\n  kind: stack\n  name: dev\n")
	mustWriteFile(t, filepath.Join(dir, "template", "angee.yaml.jinja"), strings.ReplaceAll(oneServiceTemplate, ".copier-answers.yml", ".copier-answers.stack.yml"))
	if err := os.Remove(filepath.Join(dir, "template", "ENTRIES.jinja")); err != nil {
		t.Fatal(err)
	}
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root) error = %v", err)
	}
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Overwrite: true}); err != nil {
		t.Fatalf("StackUpdateFromTemplate() error = %v", err)
	}
}

// A recorded list that no longer reads as one (edited by hand, here) refuses
// a replay, is still shown as recorded, and does not stand in the way of the
// list that replaces it.
func TestStackUpdateReplacesARecordedListThatNoLongerReads(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	writeRepositoryStackTemplate(t, project, repositoryStackCopier, repositoryStackManifest)
	p, err := New(project)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	initialized, err := p.StackInit(ctx, "dev", "stack", map[string]string{RepositoriesInput: `["ang-ee/angee-odoo"]`, ExtraSourcesInput: `["ang-ee/angee-arp"]`}, false)
	if err != nil {
		t.Fatalf("StackInit() error = %v", err)
	}
	answersPath := filepath.Join(initialized.Root, ".copier-answers.stack.yml")
	recorded, err := os.ReadFile(answersPath)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, answersPath, strings.Replace(string(recorded), "name: angee-odoo", "name: Odoo", 1))
	stackPlatform, err := New(initialized.Root)
	if err != nil {
		t.Fatalf("New(stack root) error = %v", err)
	}

	_, err = stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{})
	var invalid *InvalidInputError
	if err == nil || !errors.As(err, &invalid) || invalid.Field != RepositoriesInput || !strings.Contains(err.Error(), `the repository is named "angee-odoo" by its path, not "Odoo"`) {
		t.Fatalf("StackUpdateFromTemplate(replay) error = %v, want the recorded list refused", err)
	}
	// Replacing the other list does not make this one readable.
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{ExtraSourcesInput: `[]`}}); err == nil || !errors.As(err, &invalid) || invalid.Field != RepositoriesInput {
		t.Fatalf("StackUpdateFromTemplate(other list) error = %v, want the recorded list refused", err)
	}
	described, err := stackPlatform.StackTemplateInputs(ctx)
	if err != nil || !strings.Contains(described.Recorded[RepositoriesInput], `"name":"Odoo"`) {
		t.Fatalf("StackTemplateInputs() = %v, %v; want the list as recorded", described.Recorded, err)
	}

	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{Inputs: map[string]string{RepositoriesInput: `["ang-ee/angee-odoo@next"]`}}); err != nil {
		t.Fatalf("StackUpdateFromTemplate(replacement) error = %v", err)
	}
	described, err = stackPlatform.StackTemplateInputs(ctx)
	want := `[{"name":"angee-odoo","ref":"next","repo":"https://github.com/ang-ee/angee-odoo.git","ssh":false}]`
	if err != nil || described.Recorded[RepositoriesInput] != want {
		t.Fatalf("recorded repositories after the replacement = %v, %v; want %s", described.Recorded, err, want)
	}
	if _, err := stackPlatform.StackUpdateFromTemplate(ctx, StackUpdateTemplateOptions{}); err != nil {
		t.Fatalf("StackUpdateFromTemplate(replay after the replacement) error = %v", err)
	}
}
