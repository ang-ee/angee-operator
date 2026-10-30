package operator

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// operatorJJ is a workspace whose slot work is a real jj slot of the
// operator git fixture's cache, on branch feature-a-work, beside its git
// worktree slot app.
type operatorJJ struct {
	t                       *testing.T
	root, slot, cache, seed string
	remote, jjBin           string
}

// setupOperatorJJSlot builds operatorJJ, with a HOME of its own, as the
// service's jj tests do. It skips without jj slots on this machine, and
// fails then under ANGEE_REQUIRE_JJ=1 (CI's jj job).
func setupOperatorJJSlot(t *testing.T) *operatorJJ {
	t.Helper()
	home := t.TempDir()
	config := filepath.Join(home, "jj-config.toml")
	writeTestFile(t, config, "")
	writeTestFile(t, filepath.Join(home, ".gitconfig"), "")
	for key, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "JJ_CONFIG": config,
		"JJ_USER": "test", "JJ_EMAIL": "test@example.invalid",
		"GIT_CONFIG_GLOBAL": filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
		"GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
	} {
		t.Setenv(key, value)
	}
	if err := jjdriver.Probe(context.Background()).Err(); err != nil {
		if os.Getenv("ANGEE_REQUIRE_JJ") == "1" {
			t.Fatalf("ANGEE_REQUIRE_JJ=1, but %v", err)
		}
		t.Skipf("%v", err)
	}
	jjBin, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("LookPath(jj) error = %v", err)
	}
	root, _, _, cache := setupOperatorGitWorkspace(t)
	f := &operatorJJ{t: t, root: root, cache: cache, slot: filepath.Join(root, "workspaces", "feature-a", "work"),
		remote: filepath.Join(filepath.Dir(cache), "remote.git"), seed: filepath.Join(t.TempDir(), "seed"), jjBin: jjBin}
	f.jj(cache, "git", "init", "--colocate")
	f.jj(cache, "workspace", "add", "--colocate", "--name", "test--feature-a", "-r", "main", f.slot)
	declareOperatorSlot(t, root, "feature-a", "work", manifest.WorkspaceSource{Source: "app", Mode: manifest.WorkspaceSourceModeJJ, Branch: "feature-a-work", Ref: "main", Subpath: "work"})
	runTestGit(t, "", "clone", "-q", "-o", "fork", f.remote, f.seed)
	return f
}

// jj runs the real jj in dir, failing the test when it fails.
func (f *operatorJJ) jj(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.jjBin, append([]string{"--no-pager", "--color", "never"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("jj %q in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// commit records name with content in the slot as a commit with message.
func (f *operatorJJ) commit(name, content, message string) {
	f.t.Helper()
	writeTestFile(f.t, filepath.Join(f.slot, name), content)
	f.jj(f.slot, "commit", "-m", message)
}

// seedPush commits name with content on branch in the seed clone, from what
// the remote has of it, and pushes it there.
func (f *operatorJJ) seedPush(branch, name, content string) {
	f.t.Helper()
	runTestGit(f.t, f.seed, "fetch", "-q", "fork")
	runTestGit(f.t, f.seed, "switch", "-q", "-C", "seed", "fork/"+branch)
	writeTestFile(f.t, filepath.Join(f.seed, name), content)
	runTestGit(f.t, f.seed, "add", ".")
	runTestGit(f.t, f.seed, "commit", "-q", "-m", "seed "+name)
	runTestGit(f.t, f.seed, "push", "-q", "fork", "HEAD:"+branch)
}

// moveOnPrepare puts a jj on PATH that commits in the slot before every
// second snapshotting `jj op log`, which is publish's re-read of the slot
// before it sets the bookmark (the first is its plan's), so the slot moves
// between the plan and the push, deterministically, once per publish.
func (f *operatorJJ) moveOnPrepare() {
	f.t.Helper()
	bin := f.t.TempDir()
	count := filepath.Join(bin, "count")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do [ "$a" = --ignore-working-copy ] && exec %[1]q "$@"; done
case " $* " in *" op log "*)
	n=$(cat %[2]q 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > %[2]q
	if [ $((n %% 2)) = 0 ]; then echo "$n" > moved-$n.txt; %[1]q --no-pager commit -m "moved $n" >/dev/null 2>&1; fi;;
esac
exec %[1]q "$@"
`, f.jjBin, count)
	writeTestFile(f.t, filepath.Join(bin, "jj"), script)
	if err := os.Chmod(filepath.Join(bin, "jj"), 0o755); err != nil {
		f.t.Fatalf("Chmod() error = %v", err)
	}
	f.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A jj slot's publish refusals report the same code, cause and context over
// GraphQL (extensions) and REST (body and status): commits jj will not push
// (conflicted_commits, undescribed_commits), a branch that moved on the
// remote since the slot last published (branch_moved), a branch that exists
// and is not the slot's, and a slot that moved while its publish was checked
// (CONFLICT).
func TestJJPublishErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		branch     string
		prepare    func(f *operatorJJ)
		wantStatus int
		wantExt    map[string]any
		wantPrefix string
	}{
		{
			name: "undescribed commits",
			prepare: func(f *operatorJJ) {
				writeTestFile(f.t, filepath.Join(f.slot, "g.txt"), "undescribed\n")
				f.jj(f.slot, "new")
			},
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "PRECONDITION_FAILED", "cause": "undescribed_commits", "operation": "workspace.source-publish", "workspace": "feature-a", "slot": "work", "source": "app"},
			wantPrefix: `Publish for workspace "feature-a" slot "work": source slot "work" was not pushed: commits jj will not push: no description on `,
		},
		{
			name: "conflicted commits",
			prepare: func(f *operatorJJ) {
				f.commit("README.md", "slot\n", "slot change")
				f.seedPush("main", "README.md", "upstream\n")
				f.jj(f.slot, "git", "fetch", "--all-remotes")
				f.jj(f.slot, "rebase", "-b", "@", "--onto", "main@fork")
			},
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "PRECONDITION_FAILED", "cause": "conflicted_commits", "operation": "workspace.source-publish", "workspace": "feature-a", "slot": "work", "source": "app"},
			wantPrefix: `Publish for workspace "feature-a" slot "work": source slot "work" was not pushed: commits jj will not push: conflicts in `,
		},
		{
			name: "branch moved",
			prepare: func(f *operatorJJ) {
				f.commit("g.txt", "mine\n", "slot change")
				f.jj(f.slot, "bookmark", "set", "feature-a-work", "-r", "@-")
				f.jj(f.slot, "git", "push", "--remote", "fork", "--bookmark", "feature-a-work")
				f.seedPush("feature-a-work", "c2.txt", "theirs\n")
				f.jj(f.slot, "git", "fetch", "--all-remotes")
				f.commit("h.txt", "more\n", "more")
			},
			wantStatus: http.StatusConflict,
			wantExt: map[string]any{"code": "PRECONDITION_FAILED", "cause": "branch_moved", "operation": "workspace.source-publish", "workspace": "feature-a", "slot": "work", "source": "app",
				"hint": "Take them in first with `angee workspace source merge feature-a work feature-a-work@fork`, then retry."},
			wantPrefix: `Publish for workspace "feature-a" slot "work": source slot "work" was not pushed: branch "feature-a-work" on fork holds commits the slot does not: `,
		},
		{
			name:   "branch exists",
			branch: "develop",
			prepare: func(f *operatorJJ) {
				f.jj(f.slot, "bookmark", "create", "develop", "-r", "main")
				f.commit("g.txt", "mine\n", "slot change")
			},
			wantStatus: http.StatusBadRequest,
			wantExt:    map[string]any{"code": "INVALID_INPUT", "operation": "workspace.source-publish", "workspace": "feature-a", "slot": "work"},
			wantPrefix: `branch: branch "develop" already exists and is not jj slot "work"'s: it points at commits the slot does not own; pass another branch, or the slot's own, "feature-a-work"`,
		},
		{
			name: "slot moved",
			prepare: func(f *operatorJJ) {
				f.commit("g.txt", "mine\n", "slot change")
				f.moveOnPrepare()
			},
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "CONFLICT", "operation": "workspace.source-publish", "workspace": "feature-a", "slot": "work", "source": "app", "hint": "Run publish again."},
			wantPrefix: `Publish for workspace "feature-a" slot "work": source slot "work" changed while its publish was checked, so nothing was pushed: `,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupOperatorJJSlot(t)
			tc.prepare(f)
			server, err := NewServer(Config{Root: f.root, Bind: "127.0.0.1", Port: 28095})
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			t.Cleanup(server.Close)

			args := `workspace: "feature-a", slot: "work"`
			body := `{}`
			if tc.branch != "" {
				args += fmt.Sprintf(`, branch: %q`, tc.branch)
				body = fmt.Sprintf(`{"branch":%q}`, tc.branch)
			}
			rr := serveRequest(server, graphQLRequest(t, `mutation { workspaceSourcePublish(`+args+`) { ok } }`))
			var resp struct {
				Errors []struct {
					Message    string         `json:"message"`
					Extensions map[string]any `json:"extensions"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Errors) != 1 {
				t.Fatalf("GraphQL response = %s (%v), want one error", rr.Body.String(), err)
			}
			gqlErr := resp.Errors[0]
			if !strings.HasPrefix(gqlErr.Message, tc.wantPrefix) {
				t.Fatalf("GraphQL message = %q, want it to start %q", gqlErr.Message, tc.wantPrefix)
			}
			for key, want := range tc.wantExt {
				if got := gqlErr.Extensions[key]; !reflect.DeepEqual(got, want) {
					t.Errorf("extensions[%q] = %#v, want %#v (all: %#v)", key, got, want, gqlErr.Extensions)
				}
			}
			rest := serveREST(server, http.MethodPost, "/workspaces/feature-a/sources/work/publish", body)
			var restBody api.ErrorResponse
			if err := json.Unmarshal(rest.Body.Bytes(), &restBody); err != nil {
				t.Fatalf("Unmarshal(REST error) error = %v: %s", err, rest.Body.String())
			}
			cause, _ := tc.wantExt["cause"].(string)
			if rest.Code != tc.wantStatus || restBody.Code != tc.wantExt["code"] || restBody.Cause != cause || !strings.HasPrefix(restBody.Error, tc.wantPrefix) ||
				restBody.Operation != "workspace.source-publish" {
				t.Fatalf("REST = %d %+v, want %d with the GraphQL error's code, cause and message", rest.Code, restBody, tc.wantStatus)
			}
			if out := runTestGit(t, "", "ls-remote", f.remote, "refs/heads/"+cmp.Or(tc.branch, "feature-a-work")); tc.name != "branch moved" && out != "" {
				t.Errorf("a refused publish pushed: %s", out)
			}
		})
	}
}
