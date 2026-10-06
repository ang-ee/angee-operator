package operator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

// REST and GraphQL dispatch workspace repair through the platform: a missing
// slot makes sync-base a 409 naming it, repair answers 200 with each slot's
// outcome, and a repair with a failed slot still returns its result (REST 200
// with ok=false; GraphQL data plus an error).
func TestOperatorWorkspaceRepair(t *testing.T) {
	root, workspaceName, _, _ := setupOperatorGitWorkspace(t)
	declareOperatorSlot(t, root, workspaceName, "lib", manifest.WorkspaceSource{Source: "app", Mode: manifest.WorkspaceSourceModeWorktree, Branch: "feature-a-lib", Ref: "main", Subpath: "lib"})
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 28090})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	t.Cleanup(server.Close)

	rr := serveREST(server, http.MethodPost, "/workspaces/"+workspaceName+"/sync-base", `{"method":"merge"}`)
	var errorBody api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &errorBody); err != nil {
		t.Fatalf("Unmarshal(sync-base error) error = %v: %s", err, rr.Body.String())
	}
	if rr.Code != http.StatusConflict || !strings.Contains(errorBody.Error, `source slot "lib"`) || !strings.Contains(errorBody.Error, "angee workspace repair "+workspaceName) {
		t.Fatalf("REST sync-base with a missing slot = %d %+v, want a 409 naming lib and pointing at repair", rr.Code, errorBody)
	}

	rr = serveREST(server, http.MethodPost, "/workspaces/"+workspaceName+"/repair", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("REST repair = %d, body = %s", rr.Code, rr.Body.String())
	}
	var result api.WorkspaceRepairResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(repair) error = %v: %s", err, rr.Body.String())
	}
	if !result.OK || len(result.Slots) != 2 || result.Slots[0].Action != api.WorkspaceRepairOK || result.Slots[1].Action != api.WorkspaceRepairCreated || result.Slots[1].Status == nil {
		t.Fatalf("REST repair result = %+v, want app ok and lib created with its status", result)
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", workspaceName, "lib", ".git")); err != nil {
		t.Fatalf("lib slot after REST repair: %v", err)
	}

	declareOperatorSlot(t, root, workspaceName, "ghost", manifest.WorkspaceSource{Source: "ghost", Subpath: "ghost"})
	rr = serveREST(server, http.MethodPost, "/workspaces/"+workspaceName+"/repair", "")
	result = api.WorkspaceRepairResult{}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(partial repair) error = %v: %s", err, rr.Body.String())
	}
	if rr.Code != http.StatusOK || result.OK || len(result.Slots) != 3 || result.Slots[1].Slot != "ghost" || result.Slots[1].Action != api.WorkspaceRepairFailed {
		t.Fatalf("REST repair with a failed slot = %d %+v, want 200 with ok=false and ghost failed", rr.Code, result)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `mutation { workspaceRepair(name: "feature-a") { workspace ok slots { slot action reason status { state } } } }`,
	})
	repaired, _ := resp.Data["workspaceRepair"].(map[string]any)
	if repaired == nil || repaired["ok"] != false || repaired["workspace"] != workspaceName {
		t.Fatalf("GraphQL workspaceRepair data = %#v, want the result with ok=false", resp.Data)
	}
	slots := repaired["slots"].([]any)
	ghost := slots[1].(map[string]any)
	lib := slots[2].(map[string]any)
	if ghost["action"] != api.WorkspaceRepairFailed || lib["action"] != api.WorkspaceRepairOK || lib["status"].(map[string]any)["state"] != "clean" {
		t.Fatalf("GraphQL workspaceRepair slots = %#v, want ghost failed and lib ok and clean", slots)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].(map[string]any)["message"].(string), `"ghost"`) {
		t.Fatalf("GraphQL workspaceRepair errors = %#v, want one naming ghost", resp.Errors)
	}

	rr = serveREST(server, http.MethodPost, "/workspaces/nope/repair", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("REST repair of an undeclared workspace = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
	resp = doGraphQL(t, server, map[string]any{"query": `mutation { workspaceRepair(name: "nope") { ok } }`})
	if resp.Data != nil || len(resp.Errors) != 1 {
		t.Fatalf("GraphQL workspaceRepair of an undeclared workspace = %#v, want an error and no data", resp)
	}
}

func serveREST(server *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	return rr
}

func declareOperatorSlot(t *testing.T, root, workspaceName, slot string, wsSource manifest.WorkspaceSource) {
	t.Helper()
	stack, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	workspace := stack.Workspaces[workspaceName]
	workspace.Sources[slot] = wsSource
	stack.Workspaces[workspaceName] = workspace
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile() error = %v", err)
	}
}
