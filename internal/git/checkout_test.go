package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func checkoutRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for local repository integration")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Checkout Test")
	runGit(t, dir, "config", "user.email", "checkout@example.com")
	return dir
}

func checkoutCommit(t *testing.T, dir, text string) string {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, "value.txt"), text)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", text)
	commit, err := New().Commit(t.Context(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func TestCompleteCheckoutPreservesHistoryTagsAndPinnedRecursiveSubmodules(t *testing.T) {
	config := filepath.Join(t.TempDir(), "gitconfig")
	mustWriteFile(t, config, "[protocol \"file\"]\n\tallow = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	leaf := checkoutRepository(t)
	leafPin := checkoutCommit(t, leaf, "leaf old")
	runGit(t, leaf, "tag", "leaf-v1")
	checkoutCommit(t, leaf, "leaf new")
	module := checkoutRepository(t)
	runGit(t, module, "submodule", "add", leaf, "nested")
	runGit(t, filepath.Join(module, "nested"), "checkout", leafPin)
	modulePin := checkoutCommit(t, module, "module old")
	runGit(t, module, "tag", "module-v1")
	checkoutCommit(t, module, "module new")
	repository := checkoutRepository(t)
	runGit(t, repository, "submodule", "add", module, "module")
	runGit(t, filepath.Join(repository, "module"), "checkout", modulePin)
	// Shallow recommendations in an upstream declaration must not truncate
	// the repository a versioning/metadata backend receives.
	runGit(t, repository, "config", "-f", ".gitmodules", "submodule.module.shallow", "true")
	first := checkoutCommit(t, repository, "root old")
	runGit(t, repository, "tag", "-a", "v1", "-m", "version one")
	latest := checkoutCommit(t, repository, "root new")
	// Complete traversal must ignore restrictive active-submodule defaults.
	mustWriteFile(t, config, "[protocol \"file\"]\n\tallow = always\n[submodule]\n\tactive = excluded/**\n[submodule \"module\"]\n\tactive = false\n[submodule \"nested\"]\n\tactive = false\n")
	client := New()
	for _, revision := range []string{"main", "refs/heads/main", latest, "v1"} {
		t.Run(revision, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "repository")
			commit, err := client.CompleteCheckout(t.Context(), repository, dest, revision)
			if err != nil {
				t.Fatal(err)
			}
			want := latest
			if revision == "v1" {
				want = first
			}
			if commit != want {
				t.Fatalf("revision = %s, want %s", commit, want)
			}
			for _, item := range []struct{ dir, ref, want string }{
				{dest, "v1", first},
				{filepath.Join(dest, "module"), "HEAD", modulePin},
				{filepath.Join(dest, "module"), "module-v1", modulePin},
				{filepath.Join(dest, "module", "nested"), "HEAD", leafPin},
				{filepath.Join(dest, "module", "nested"), "leaf-v1", leafPin},
			} {
				actual, err := client.Commit(t.Context(), item.dir, item.ref)
				if err != nil || actual != item.want {
					t.Fatalf("%s %s = %s, %v", item.dir, item.ref, actual, err)
				}
				shallow, err := client.runText(t.Context(), item.dir, "rev-parse", "--is-shallow-repository")
				if err != nil || shallow != "false" {
					t.Fatalf("shallow = %q, %v", shallow, err)
				}
			}
			count, err := client.runText(t.Context(), dest, "rev-list", "--count", "main")
			if err != nil || count != "2" {
				t.Fatalf("history = %q, %v", count, err)
			}
			if _, err := client.Run(t.Context(), dest, "symbolic-ref", "-q", "HEAD"); err == nil {
				t.Fatal("checkout remains on a moving branch")
			}
		})
	}
}

func TestCompleteCheckoutRefusesUnsupportedRepositoryAndRevision(t *testing.T) {
	repository := checkoutRepository(t)
	checkoutCommit(t, repository, "initial")
	for _, test := range []struct{ repository, revision string }{
		{repository, "missing"}, {repository, "main:refs/heads/other"},
		{"unsupported://invalid/repository", "main"},
	} {
		if _, err := New().CompleteCheckout(t.Context(), test.repository, filepath.Join(t.TempDir(), "checkout"), test.revision); err == nil {
			t.Fatalf("accepted repository %s revision %s", test.repository, test.revision)
		}
	}
	shallow := filepath.Join(t.TempDir(), "shallow")
	runGit(t, "", "clone", "--depth", "1", "file://"+repository, shallow)
	_, err := New().CompleteCheckout(t.Context(), shallow, filepath.Join(t.TempDir(), "checkout"), "main")
	if err == nil || !strings.Contains(err.Error(), "full history") {
		t.Fatalf("shallow source = %v", err)
	}
	// The source was never changed by successful or failed checkout requests.
	if _, err := os.Stat(filepath.Join(repository, "value.txt")); err != nil {
		t.Fatal(err)
	}
}
