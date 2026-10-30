package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const remotesManifest = `version: 1
kind: stack
name: remotes
sources:
  app:
    kind: git
    remotes:
      origin: git@github.com:me/app.git
      upstream:
        url: https://github.com/acme/app.git
        canonical: true
      staging:
        url: git@example.com:acme/app.git
        branches: [release/*, "hotfix-*"]
    push_default: origin
    default_ref: main
`

func loadManifestString(t *testing.T, text string) (*Stack, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "angee.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}
	return LoadFile(path)
}

// Both forms of a remote load, and saving keeps each in the form it was
// written in: a remote with only a URL stays a bare string.
func TestSourceRemotesYAMLRoundTrip(t *testing.T) {
	stack, err := loadManifestString(t, remotesManifest)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	want := map[string]SourceRemote{
		"origin":   {URL: "git@github.com:me/app.git"},
		"upstream": {URL: "https://github.com/acme/app.git", Canonical: true},
		"staging":  {URL: "git@example.com:acme/app.git", Branches: []string{"release/*", "hotfix-*"}},
	}
	source := stack.Sources["app"]
	if !reflect.DeepEqual(source.Remotes, want) || source.PushDefault != "origin" {
		t.Fatalf("source = %#v, want remotes %#v and push_default origin", source, want)
	}

	data, err := Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	text := string(data)
	for _, line := range []string{
		"            origin: git@github.com:me/app.git\n",
		"                canonical: true\n",
		"                branches:\n",
		"        push_default: origin\n",
	} {
		if !strings.Contains(text, line) {
			t.Fatalf("Marshal() output lacks %q:\n%s", line, text)
		}
	}
	reloaded, err := loadManifestString(t, text)
	if err != nil {
		t.Fatalf("LoadFile(marshalled) error = %v", err)
	}
	if !reflect.DeepEqual(reloaded.Sources["app"], source) {
		t.Fatalf("reloaded source = %#v, want %#v", reloaded.Sources["app"], source)
	}
}

// The mapping form keeps the manifest's strictness: an unknown key is refused
// even though the remote decodes with its own decoder.
func TestSourceRemoteRejectsUnknownField(t *testing.T) {
	text := strings.Replace(remotesManifest, "        canonical: true\n", "        canonical: true\n        pushurl: nowhere\n", 1)
	if _, err := loadManifestString(t, text); err == nil || !strings.Contains(err.Error(), "field pushurl not found in a remote") {
		t.Fatalf("LoadFile() error = %v, want the unknown remote field refused", err)
	}
	text = strings.Replace(remotesManifest, "      origin: git@github.com:me/app.git\n", "      origin: [git@github.com:me/app.git]\n", 1)
	if _, err := loadManifestString(t, text); err == nil || !strings.Contains(err.Error(), "a remote is a URL or a mapping") {
		t.Fatalf("LoadFile() error = %v, want a sequence remote refused", err)
	}
}

func TestSourceRemoteJSONForms(t *testing.T) {
	var remotes map[string]SourceRemote
	if err := json.Unmarshal([]byte(`{"origin":"git@github.com:me/app.git","upstream":{"url":"https://github.com/acme/app.git","canonical":true}}`), &remotes); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := map[string]SourceRemote{
		"origin":   {URL: "git@github.com:me/app.git"},
		"upstream": {URL: "https://github.com/acme/app.git", Canonical: true},
	}
	if !reflect.DeepEqual(remotes, want) {
		t.Fatalf("remotes = %#v, want %#v", remotes, want)
	}
	if err := json.Unmarshal([]byte(`{"origin":{"url":"x","pushurl":"y"}}`), &remotes); err == nil {
		t.Fatal("json.Unmarshal(unknown field) error = nil, want it refused")
	}
}

func TestIsRelativeURL(t *testing.T) {
	for url, want := range map[string]bool{
		"../app":                                 true,
		"sources/app":                            true,
		"app":                                    true,
		"123":                                    true,
		"./dir:name":                             true,
		"host:repo.git":                          false,
		"/srv/git/app.git":                       false,
		"https://github.com/acme/app.git":        false,
		"file:///srv/git/app.git":                false,
		"git@github.com:acme/app.git":            false,
		"git@github-arpee:/ang-ee/angee-arp.git": false,
	} {
		if got := IsRelativeURL(url); got != want {
			t.Errorf("IsRelativeURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestSourceCloneRemote(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   Source
		wantName string
		wantURL  string
		wantOK   bool
	}{
		{name: "repo is origin", source: Source{Repo: "r"}, wantName: "origin", wantURL: "r", wantOK: true},
		{name: "canonical wins over origin", source: Source{Remotes: map[string]SourceRemote{"origin": {URL: "fork"}, "upstream": {URL: "up", Canonical: true}}}, wantName: "upstream", wantURL: "up", wantOK: true},
		{name: "origin without a canonical", source: Source{Remotes: map[string]SourceRemote{"origin": {URL: "fork"}, "staging": {URL: "s"}}}, wantName: "origin", wantURL: "fork", wantOK: true},
		{name: "the only remote", source: Source{Remotes: map[string]SourceRemote{"fork": {URL: "f"}}}, wantName: "fork", wantURL: "f", wantOK: true},
		{name: "several, none qualifies", source: Source{Remotes: map[string]SourceRemote{"fork": {URL: "f"}, "staging": {URL: "s"}}}},
		{name: "no remote", source: Source{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, remote, ok := tc.source.CloneRemote()
			if name != tc.wantName || remote.URL != tc.wantURL || ok != tc.wantOK {
				t.Fatalf("CloneRemote() = (%q, %q, %v), want (%q, %q, %v)", name, remote.URL, ok, tc.wantName, tc.wantURL, tc.wantOK)
			}
			if got := tc.source.CloneURL(); got != tc.wantURL {
				t.Fatalf("CloneURL() = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestValidateSource(t *testing.T) {
	const fork, up = "git@github.com:me/app.git", "https://github.com/acme/app.git"
	remotes := func(pairs ...any) map[string]SourceRemote {
		out := map[string]SourceRemote{}
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i].(string)] = pairs[i+1].(SourceRemote)
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		source  Source
		wantErr string
	}{
		{name: "repo alone", source: Source{Kind: "git", Repo: "r"}},
		{name: "relative repo", source: Source{Kind: "git", Repo: "../app"}},
		{name: "repo with push_default origin", source: Source{Kind: "git", Repo: "r", PushDefault: "origin"}},
		{name: "no remote at all", source: Source{Kind: "git", CachePath: "/srv/app"}},
		{name: "fork and canonical upstream", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: fork, Branches: []string{"release/*"}}, "upstream", SourceRemote{URL: up, Canonical: true}), PushDefault: "origin"}},
		{name: "scp-like and absolute urls", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: "git@github.com:me/app.git"}, "local", SourceRemote{URL: "/srv/git/app.git"})}},
		{name: "repo and remotes", source: Source{Kind: "git", Repo: "r", Remotes: remotes("origin", SourceRemote{URL: fork})}, wantErr: "set repo or remotes, not both"},
		{name: "remotes on a local source", source: Source{Kind: "local", Remotes: remotes("origin", SourceRemote{URL: fork})}, wantErr: "require kind: git"},
		{name: "push_default on a local source", source: Source{Kind: "local", PushDefault: "origin"}, wantErr: "require kind: git"},
		{name: "option-like remote name", source: Source{Kind: "git", Remotes: remotes("--mirror", SourceRemote{URL: fork})}, wantErr: `remote name "--mirror"`},
		{name: "remote name with a slash", source: Source{Kind: "git", Remotes: remotes("a/b", SourceRemote{URL: fork})}, wantErr: `remote name "a/b"`},
		{name: "remote name ending .lock", source: Source{Kind: "git", Remotes: remotes("origin.lock", SourceRemote{URL: fork})}, wantErr: `remote name "origin.lock"`},
		{name: "remote names differing only in case", source: Source{Kind: "git", Remotes: remotes("Origin", SourceRemote{URL: fork}, "origin", SourceRemote{URL: up})}, wantErr: `remote names "Origin" and "origin" differ only in case`},
		{name: "remote without a url", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{})}, wantErr: `remote "origin": url is required`},
		{name: "blank url", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: " "})}, wantErr: `remote "origin": url has leading or trailing spaces`},
		{name: "url with surrounding spaces", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: " " + fork + " "})}, wantErr: "has leading or trailing spaces"},
		{name: "url with a newline", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: fork + "\n" + up})}, wantErr: `remote "origin": url contains a control character`},
		{name: "repo with a newline", source: Source{Kind: "git", Repo: fork + "\n" + up}, wantErr: "repo contains a control character"},
		{name: "relative url under remotes", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: "../fork.git"})}, wantErr: `url "../fork.git" is a relative path`},
		{name: "number as url", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: "123"})}, wantErr: "is a relative path"},
		{name: "two canonical remotes", source: Source{Kind: "git", Remotes: remotes("a", SourceRemote{URL: fork, Canonical: true}, "b", SourceRemote{URL: up, Canonical: true})}, wantErr: "both canonical"},
		{name: "branches on the canonical remote", source: Source{Kind: "git", Remotes: remotes("upstream", SourceRemote{URL: up, Canonical: true, Branches: []string{"release/*"}})}, wantErr: "a canonical remote is fetch-only, so branches has nothing to allow"},
		{name: "bad branches pattern", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: fork, Branches: []string{"release/["}})}, wantErr: `branches pattern "release/["`},
		{name: "empty branches pattern", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: fork, Branches: []string{""}})}, wantErr: `branches pattern ""`},
		{name: "push_default undeclared", source: Source{Kind: "git", Remotes: remotes("origin", SourceRemote{URL: fork}), PushDefault: "fork"}, wantErr: `push_default "fork" names no declared remote`},
		{name: "push_default undeclared with repo", source: Source{Kind: "git", Repo: "r", PushDefault: "fork"}, wantErr: `push_default "fork" names no declared remote`},
		{name: "push_default canonical", source: Source{Kind: "git", Remotes: remotes("upstream", SourceRemote{URL: up, Canonical: true}), PushDefault: "upstream"}, wantErr: "is the canonical remote"},
		{name: "clone remote undeterminable", source: Source{Kind: "git", Remotes: remotes("fork", SourceRemote{URL: fork}, "staging", SourceRemote{URL: up})}, wantErr: "cannot tell which remote to clone from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSource("app", tc.source)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSource() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), `source "app"`) {
				t.Fatalf("ValidateSource() error = %v, want one naming the source and containing %q", err, tc.wantErr)
			}
		})
	}
}

// LoadFile runs ValidateSource on every source.
func TestLoadFileValidatesSourceRemotes(t *testing.T) {
	text := strings.Replace(remotesManifest, "    push_default: origin\n", "    push_default: upstream\n", 1)
	if _, err := loadManifestString(t, text); err == nil || !strings.Contains(err.Error(), "is the canonical remote") {
		t.Fatalf("LoadFile() error = %v, want push_default to the canonical remote refused", err)
	}
}

// YAML shapes a remote can take on its way through LoadFile (from the review
// of #116): what each loads as, or why it is refused.
func TestSourceRemoteYAMLEdges(t *testing.T) {
	const head = "version: 1\nkind: stack\nname: r\nsources:\n  app:\n    kind: git\n    remotes:\n"
	for _, tc := range []struct {
		name    string
		body    string
		want    map[string]SourceRemote
		wantErr string
	}{
		{name: "null", body: "      origin: ~\n", wantErr: `remote "origin": url is required`},
		{name: "empty", body: "      origin:\n", wantErr: `remote "origin": url is required`},
		{name: "number", body: "      origin: 123\n", wantErr: `url "123" is a relative path`},
		{name: "flow mapping", body: "      origin: {url: https://x/a.git}\n", want: map[string]SourceRemote{"origin": {URL: "https://x/a.git"}}},
		{
			name: "merge key",
			body: "      origin: &o {url: https://x/a.git}\n      fork:\n        <<: *o\n        branches: [a]\n",
			want: map[string]SourceRemote{"origin": {URL: "https://x/a.git"}, "fork": {URL: "https://x/a.git", Branches: []string{"a"}}},
		},
		{
			name:    "flow mapping with an unknown field",
			body:    "      origin: {url: https://x/a.git, pushurl: y}\n",
			wantErr: "field pushurl not found in a remote",
		},
		{
			name:    "merged mapping with an unknown field",
			body:    "      fork:\n        <<: {url: https://x/a.git, pushurl: y}\n",
			wantErr: "field pushurl not found in a remote",
		},
		{name: "surrounding spaces", body: "      origin: \" https://x/a.git \"\n", wantErr: "url has leading or trailing spaces"},
		{name: "newline", body: "      origin: \"https://x/a.git\\nhttps://y/b.git\"\n", wantErr: "url contains a control character"},
		{name: "names differing in case", body: "      Origin: https://x/a.git\n      origin: https://y/b.git\n", wantErr: "differ only in case"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack, err := loadManifestString(t, head+tc.body)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadFile() error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFile() error = %v", err)
			}
			if got := stack.Sources["app"].Remotes; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("remotes = %#v, want %#v", got, tc.want)
			}
		})
	}
}
