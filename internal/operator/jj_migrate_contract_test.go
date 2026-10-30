package operator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

// A migrate's refusals report the same code, cause and context over GraphQL
// (extensions) and REST (body and status): a jj slot's files jj does not
// track (untracked_paths) and its uncommitted changes, a git worktree's
// changes to tracked files (uncommitted_changes), and a mode migrate does
// not know (INVALID_INPUT on to).
func TestMigrateErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name, slot, to string
		prepare        func(f *operatorJJ)
		wantStatus     int
		wantExt        map[string]any
		wantPrefix     string
	}{
		{
			name: "untracked paths", slot: "work",
			prepare: func(f *operatorJJ) {
				f.jj(f.cache, "config", "set", "--repo", "snapshot.max-new-file-size", "16")
				writeTestFile(f.t, filepath.Join(f.slot, "big.bin"), strings.Repeat("x", 200))
			},
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "PRECONDITION_FAILED", "cause": "untracked_paths", "operation": "workspace.source-migrate", "workspace": "feature-a", "slot": "work", "source": "app", "paths": []any{"big.bin"}},
			wantPrefix: `Migrate for workspace "feature-a" slot "work": source slot "work" holds files jj does not track, only it has, and migrate leaves out, since no commit holds them: big.bin.`,
		},
		{
			name: "jj changes", slot: "work",
			prepare:    func(f *operatorJJ) { writeTestFile(f.t, filepath.Join(f.slot, "wip.txt"), "wip\n") },
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "PRECONDITION_FAILED", "cause": "uncommitted_changes", "operation": "workspace.source-migrate", "workspace": "feature-a", "slot": "work", "source": "app"},
			wantPrefix: `Migrate for workspace "feature-a" slot "work": source slot "work" has uncommitted changes in wip.txt.`,
		},
		{
			name: "worktree changes", slot: "app",
			prepare: func(f *operatorJJ) {
				writeTestFile(f.t, filepath.Join(f.root, "workspaces", "feature-a", "app", "README.md"), "changed\n")
			},
			wantStatus: http.StatusConflict,
			wantExt:    map[string]any{"code": "PRECONDITION_FAILED", "cause": "uncommitted_changes", "operation": "workspace.source-migrate", "workspace": "feature-a", "slot": "app", "source": "app"},
			wantPrefix: `Migrate for workspace "feature-a" slot "app": source slot "app" has uncommitted changes in README.md.`,
		},
		{
			name: "unknown mode", slot: "app", to: "hg",
			prepare:    func(*operatorJJ) {},
			wantStatus: http.StatusBadRequest,
			wantExt:    map[string]any{"code": "INVALID_INPUT", "operation": "workspace.source-migrate", "workspace": "feature-a", "slot": "app"},
			wantPrefix: `to: migrate moves a slot to mode "jj" or "worktree", not "hg"`,
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

			args := fmt.Sprintf(`workspace: "feature-a", slot: %q`, tc.slot)
			body := `{}`
			if tc.to != "" {
				args += fmt.Sprintf(`, to: %q`, tc.to)
				body = fmt.Sprintf(`{"to":%q}`, tc.to)
			}
			rr := serveRequest(server, graphQLRequest(t, `mutation { workspaceSourceMigrate(`+args+`) { ok } }`))
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
			rest := serveREST(server, http.MethodPost, "/workspaces/feature-a/sources/"+tc.slot+"/migrate", body)
			var restBody api.ErrorResponse
			if err := json.Unmarshal(rest.Body.Bytes(), &restBody); err != nil {
				t.Fatalf("Unmarshal(REST error) error = %v: %s", err, rest.Body.String())
			}
			cause, _ := tc.wantExt["cause"].(string)
			if rest.Code != tc.wantStatus || restBody.Code != tc.wantExt["code"] || restBody.Cause != cause || !strings.HasPrefix(restBody.Error, tc.wantPrefix) ||
				restBody.Operation != "workspace.source-migrate" {
				t.Fatalf("REST = %d %+v, want %d with the GraphQL error's code, cause and message", rest.Code, restBody, tc.wantStatus)
			}
		})
	}
	// A misspelled field is refused, not read as the default direction.
	f := setupOperatorJJSlot(t)
	server, err := NewServer(Config{Root: f.root, Bind: "127.0.0.1", Port: 28095})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	if rest := serveREST(server, http.MethodPost, "/workspaces/feature-a/sources/app/migrate", `{"mode":"jj"}`); rest.Code != http.StatusBadRequest {
		t.Fatalf("REST migrate with an unknown field = %d %s, want 400", rest.Code, rest.Body.String())
	}
}

// A migrate over GraphQL answers with the slot's status after it, and the
// REST route moves it back.
func TestMigrateOverTheOperator(t *testing.T) {
	f := setupOperatorJJSlot(t)
	server, err := NewServer(Config{Root: f.root, Bind: "127.0.0.1", Port: 28095})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)
	rr := serveRequest(server, graphQLRequest(t, `mutation { workspaceSourceMigrate(workspace: "feature-a", slot: "app") { ok message source { mode } } }`))
	var resp struct {
		Data struct {
			Migrate struct {
				OK      bool   `json:"ok"`
				Message string `json:"message"`
				Source  struct {
					Mode string `json:"mode"`
				} `json:"source"`
			} `json:"workspaceSourceMigrate"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || len(resp.Errors) != 0 || !resp.Data.Migrate.OK || resp.Data.Migrate.Source.Mode != "jj" ||
		!strings.Contains(resp.Data.Migrate.Message, `jj workspace "test--feature-a--app"`) {
		t.Fatalf("GraphQL migrate = %s (%v), want the slot a jj slot", rr.Body.String(), err)
	}
	rest := serveREST(server, http.MethodPost, "/workspaces/feature-a/sources/app/migrate", `{"to":"worktree"}`)
	var result api.GitOpResult
	if err := json.Unmarshal(rest.Body.Bytes(), &result); err != nil || rest.Code != http.StatusOK || !result.OK || result.Source == nil || result.Source.Mode != "worktree" {
		t.Fatalf("REST migrate back = %d %s (%v), want the slot a worktree", rest.Code, rest.Body.String(), err)
	}
}
