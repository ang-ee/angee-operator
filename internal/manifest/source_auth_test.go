package manifest

import (
	"strings"
	"testing"
)

const slotAndKeyManifest = `version: 1
kind: stack
name: repositories
sources:
  angee-arp:
    kind: git
    repo: git@github.com:ang-ee/angee-arp.git
    default_ref: main
    cache_path: sources/angee-arp
    slot: true
    auth:
      ssh_key_secret: deploy-key-angee-arp
  plain:
    kind: git
    repo: https://github.com/acme/plain.git
secrets:
  deploy-key-angee-arp:
    required: true
`

// A source's slot mark and its key load, and a save writes them back and
// adds neither to a source that has none.
func TestSourceSlotAndKeyRoundTrip(t *testing.T) {
	stack, err := loadManifestString(t, slotAndKeyManifest)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if source := stack.Sources["angee-arp"]; !source.Slot || source.Auth.SSHKeySecret != "deploy-key-angee-arp" {
		t.Fatalf("source angee-arp = %+v, want its slot mark and its key", source)
	}
	if source := stack.Sources["plain"]; source.Slot || source.Auth != (SourceAuth{}) {
		t.Fatalf("source plain = %+v, want no slot mark and no auth", source)
	}
	saved, err := Marshal(stack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, line := range []string{"        slot: true\n", "        auth:\n            ssh_key_secret: deploy-key-angee-arp\n"} {
		if strings.Count(string(saved), line) != 1 {
			t.Fatalf("saved manifest holds %q %d times, want once:\n%s", line, strings.Count(string(saved), line), saved)
		}
	}
}

// `auth.mode` was declared and never read; it is gone, so a manifest that
// sets it fails strict loading, as any unknown field does.
func TestSourceAuthModeIsNoField(t *testing.T) {
	_, err := loadManifestString(t, strings.Replace(slotAndKeyManifest, "      ssh_key_secret: deploy-key-angee-arp\n", "      mode: ssh\n", 1))
	if err == nil || !strings.Contains(err.Error(), "field mode not found") {
		t.Fatalf("LoadFile() error = %v, want auth.mode refused", err)
	}
}

// A secret that is a source's SSH key cannot be generated: a random string
// is no key, and resolving the stack's secrets would make and keep one.
func TestKeySecretIsNotGenerated(t *testing.T) {
	_, err := loadManifestString(t, strings.Replace(slotAndKeyManifest, "    required: true\n", "    generated: true\n", 1))
	want := `secret "deploy-key-angee-arp" is the SSH key of source "angee-arp" (auth.ssh_key_secret), so it cannot be generated`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadFile() error = %v, want %q", err, want)
	}
	// A generated secret no source takes for its key is as before.
	if _, err := loadManifestString(t, slotAndKeyManifest+"  other:\n    generated: true\n"); err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

// Two sources that each name a key have their key files side by side, named
// after them: names that differ only in case would be one file where the
// file system folds case.
func TestKeyedSourceNamesDifferByMoreThanCase(t *testing.T) {
	two := strings.Replace(slotAndKeyManifest, "  plain:\n", "  Angee-ARP:\n    kind: git\n    repo: git@github.com:fork/angee-arp.git\n    auth:\n      ssh_key_secret: other-key\n  plain:\n", 1)
	_, err := loadManifestString(t, two)
	want := `sources "Angee-ARP" and "angee-arp" each name an SSH key (auth.ssh_key_secret) and differ only in case, so their key files would be one; rename one of them`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadFile() error = %v, want %q", err, want)
	}
	// Without a key of its own the second is as free as before.
	if _, err := loadManifestString(t, strings.Replace(two, "    auth:\n      ssh_key_secret: other-key\n", "", 1)); err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestIsSSHURL(t *testing.T) {
	for url, want := range map[string]bool{
		"git@github.com:acme/app.git":         true,
		"deploy:app":                          true,
		"ssh://git@github.com/acme/app.git":   true,
		"git+ssh://git@example.com/a/app.git": true,
		"SSH://git@example.com/a/app.git":     true,
		"https://github.com/acme/app.git":     false,
		"file:///srv/git/app.git":             false,
		"/srv/git/app.git":                    false,
		"../app":                              false,
		"app":                                 false,
		"":                                    false,
	} {
		if got := IsSSHURL(url); got != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestValidateSourceAuth(t *testing.T) {
	git := func(name string) Source {
		return Source{Kind: "git", Repo: "git@example.com:acme/app.git", Auth: SourceAuth{SSHKeySecret: name}}
	}
	for _, tc := range []struct {
		name   string
		source Source
		want   string // "" when valid
	}{
		{name: "app", source: git("deploy-key-app")},
		{name: "app", source: git("Deploy.Key_app-2")},
		// token_secret is declared and reserved; setting it is no error.
		{name: "app", source: Source{Kind: "git", Repo: "https://example.com/acme/app.git", Auth: SourceAuth{TokenSecret: "token"}}},
		{name: "app", source: Source{Kind: "local", Path: "/srv/app", Auth: SourceAuth{SSHKeySecret: "key"}}, want: `source "app": auth.ssh_key_secret requires kind: git`},
		{name: "app", source: git("deploy key"), want: `source "app": auth.ssh_key_secret "deploy key" is not a secret name`},
		{name: "app", source: git("../key"), want: `source "app": auth.ssh_key_secret "../key" is not a secret name`},
		{name: "app", source: git("--"), want: `source "app": auth.ssh_key_secret "--" is not a secret name`},
		{name: "known_hosts", source: git("key"), want: `source "known_hosts": a source with auth.ssh_key_secret has its key file named after it, so its name cannot be "." or ".." or start with "known_hosts"; rename the source`},
		{name: "Known_Hosts", source: git("key"), want: `source "Known_Hosts": a source with auth.ssh_key_secret has its key file named after it`},
		{name: "known_hosts.old", source: git("key"), want: `source "known_hosts.old": a source with auth.ssh_key_secret has its key file named after it`},
		// The key is read only over ssh.
		{name: "app", source: Source{Kind: "git", Repo: "https://example.com/acme/app.git", Auth: SourceAuth{SSHKeySecret: "key"}}, want: `source "app": auth.ssh_key_secret is set, but none of its remotes is reached over ssh (git@host:path or ssh://…), so the key would never be used`},
		{name: "app", source: Source{Kind: "git", Repo: "/srv/git/app.git", Auth: SourceAuth{SSHKeySecret: "key"}}, want: `none of its remotes is reached over ssh`},
		{name: "app", source: Source{Kind: "git", Auth: SourceAuth{SSHKeySecret: "key"}}, want: `none of its remotes is reached over ssh`},
		{name: "app", source: Source{Kind: "git", Repo: "ssh://git@example.com/acme/app.git", Auth: SourceAuth{SSHKeySecret: "key"}}},
		{name: "app", source: Source{Kind: "git", Repo: "GIT+SSH://git@example.com/acme/app.git", Auth: SourceAuth{SSHKeySecret: "key"}}},
		// One ssh remote among several is enough: the key is for that one.
		{name: "app", source: Source{Kind: "git", Remotes: map[string]SourceRemote{"origin": {URL: "git@example.com:me/app.git"}, "upstream": {URL: "https://example.com/acme/app.git", Canonical: true}}, Auth: SourceAuth{SSHKeySecret: "key"}}},
		{name: "..", source: git("key"), want: `source "..": a source with auth.ssh_key_secret has its key file named after it`},
		// Without a key the name is as free as before.
		{name: "known_hosts", source: Source{Kind: "git", Repo: "git@example.com:acme/app.git"}},
	} {
		err := ValidateSource(tc.name, tc.source)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("ValidateSource(%s, %+v) error = %v, want none", tc.name, tc.source.Auth, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("ValidateSource(%s, %+v) error = %v, want %q", tc.name, tc.source.Auth, err, tc.want)
		}
	}
}
