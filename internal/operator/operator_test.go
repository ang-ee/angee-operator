package operator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
)

func TestNewServerRequiresTokenForNonLoopbackBind(t *testing.T) {
	_, err := NewServer(Config{Root: t.TempDir(), Bind: "0.0.0.0", Port: 9000})
	if err == nil {
		t.Fatal("NewServer() error = nil, want token requirement")
	}
}

func TestNewServerResolvesProjectRootToControlRoot(t *testing.T) {
	projectRoot := t.TempDir()
	controlRoot := filepath.Join(projectRoot, ".angee")
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	server, err := NewServer(Config{Root: projectRoot, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.config.Root != controlRoot {
		t.Fatalf("server root = %q, want %q", server.config.Root, controlRoot)
	}
}

func TestNewServerDiscoversParentControlRoot(t *testing.T) {
	projectRoot := t.TempDir()
	controlRoot := filepath.Join(projectRoot, ".angee")
	nested := filepath.Join(projectRoot, "app", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll(nested) error = %v", err)
	}
	if err := os.MkdirAll(controlRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(.angee) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(controlRoot, "angee.yaml"), []byte("version: 1\nkind: stack\nname: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}

	server, err := NewServer(Config{Root: nested, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.config.Root != controlRoot {
		t.Fatalf("server root = %q, want %q", server.config.Root, controlRoot)
	}
}

func TestGraphQLStackStatus(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  api:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ stackStatus { name services { name runtime status } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	status := resp.Data["stackStatus"].(map[string]any)
	if status["name"] != "test" {
		t.Fatalf("stackStatus.name = %v, want test", status["name"])
	}
	services := status["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("services length = %d, want 1", len(services))
	}
	service := services[0].(map[string]any)
	if service["name"] != "api" || service["runtime"] != "container" || service["status"] != "declared" {
		t.Fatalf("service = %#v, want declared api container", service)
	}
}

func TestGraphQLWorkspaceStatus(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
sources:
  app:
    kind: git
    repo: https://example.invalid/app.git
workspaces:
  feat:
    template: workspaces/dev-pr
    inputs:
      topic: feature
    sources:
      app:
        source: app
        mode: worktree
        branch: workspace/feat
        ref: main
        subpath: app
    resolved:
      allocations:
        custom: 10002
        playwright: 9225
services:
  worker:
    runtime: local
    command: ["true"]
    mounts: ["workspace://feat:/workspace"]
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ workspaceStatus(name: "feat") { name state exists inputs processComposePort playwrightMcpName playwrightMcpUrl sources { slot source kind mode state pushed } mountedBy { kind name field } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	status := resp.Data["workspaceStatus"].(map[string]any)
	if status["name"] != "feat" || status["state"] != "missing" || status["exists"] != false {
		t.Fatalf("workspaceStatus = %#v, want missing feat", status)
	}
	if status["processComposePort"] != float64(10002) || status["playwrightMcpName"] != "playwright-feat" || status["playwrightMcpUrl"] != "http://127.0.0.1:9225/mcp" {
		t.Fatalf("workspace runtime facts = %#v, want control/MCP facts", status)
	}
	sources := status["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources length = %d, want 1", len(sources))
	}
	source := sources[0].(map[string]any)
	if source["slot"] != "app" || source["source"] != "app" || source["kind"] != "git" || source["mode"] != "worktree" || source["state"] != "missing" {
		t.Fatalf("source status = %#v, want missing app git worktree", source)
	}
	mountedBy := status["mountedBy"].([]any)
	if len(mountedBy) != 1 {
		t.Fatalf("mountedBy length = %d, want 1", len(mountedBy))
	}
	ref := mountedBy[0].(map[string]any)
	if ref["kind"] != "service" || ref["name"] != "worker" || ref["field"] != "mounts" {
		t.Fatalf("mountedBy = %#v, want worker mounts ref", ref)
	}
}

func TestGraphQLGitOpsTopologyAndWorkspaceSourcePush(t *testing.T) {
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	workspaceSourcePath := filepath.Join(root, "workspaces", workspaceName, "app")

	runTestGit(t, "", "init", "--bare", remote)
	runTestGit(t, "", "clone", remote, cache)
	runTestGit(t, cache, "remote", "rename", "origin", "fork")
	runTestGit(t, cache, "config", "user.email", "test@example.com")
	runTestGit(t, cache, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(cache, "README.md"), "hello\n")
	runTestGit(t, cache, "add", "README.md")
	runTestGit(t, cache, "commit", "-m", "initial")
	runTestGit(t, cache, "branch", "-M", "main")
	runTestGit(t, cache, "push", "-u", "fork", "main")
	runTestGit(t, cache, "worktree", "add", "-b", workspaceName, workspaceSourcePath, "main")
	runTestGit(t, workspaceSourcePath, "config", "user.email", "test@example.com")
	runTestGit(t, workspaceSourcePath, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(workspaceSourcePath, "change.txt"), "change\n")
	writeTestStack(t, workspaceSourcePath, `version: 1
kind: stack
name: inner
`)
	runTestGit(t, workspaceSourcePath, "add", "change.txt", "angee.yaml")
	runTestGit(t, workspaceSourcePath, "commit", "-m", "workspace change")

	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {
				Kind:       "git",
				Repo:       remote,
				DefaultRef: "main",
				CachePath:  cache,
			},
		},
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {
						Source:  "app",
						Mode:    "worktree",
						Branch:  workspaceName,
						Ref:     "main",
						Subpath: "app",
					},
				},
				Resolved: manifest.WorkspaceResolved{
					ChainRoot: "app",
				},
			},
		},
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{
			gitOpsTopology {
				name
				summary { sources workspaces worktrees ahead unpushed }
				sources { name state ref pushed }
				links { id workspace slot source state ahead pushed currentRef }
				workspaces {
					name
					sources { slot state ahead pushed }
					innerStack { name services { name } jobs { name } workspaces { name } }
				}
			}
		}`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	topology := resp.Data["gitOpsTopology"].(map[string]any)
	summary := topology["summary"].(map[string]any)
	if summary["sources"] != float64(1) || summary["workspaces"] != float64(1) || summary["worktrees"] != float64(1) {
		t.Fatalf("summary = %#v, want one source, workspace, and worktree", summary)
	}
	if summary["ahead"] != float64(1) || summary["unpushed"] != float64(1) {
		t.Fatalf("summary = %#v, want one ahead unpushed worktree", summary)
	}
	links := topology["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links length = %d, want 1", len(links))
	}
	link := links[0].(map[string]any)
	if link["workspace"] != workspaceName || link["slot"] != "app" || link["state"] != "ahead" || link["ahead"] != float64(1) || link["pushed"] != false {
		t.Fatalf("link = %#v, want ahead unpushed app worktree", link)
	}

	resp = doGraphQL(t, server, map[string]any{
		"query": `mutation {
			workspaceSourcePush(workspace: "feature-a", slot: "app") {
				slot
				state
				pushed
				upstream
				ahead
			}
		}`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL push errors = %#v", resp.Errors)
	}
	pushed := resp.Data["workspaceSourcePush"].(map[string]any)
	if pushed["slot"] != "app" || pushed["state"] != "clean" || pushed["pushed"] != true || pushed["upstream"] != "fork/"+workspaceName {
		t.Fatalf("workspaceSourcePush = %#v, want clean pushed fork upstream", pushed)
	}
}

func TestOperatorWorkspaceBranchIdentityStatusAndSyncBase(t *testing.T) {
	root, workspaceName, workspaceSourcePath, cache := setupOperatorGitWorkspace(t)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	runTestGit(t, workspaceSourcePath, "switch", "-c", "codex/feature-a")
	req := httptest.NewRequest(http.MethodGet, "/workspaces/"+workspaceName+"/status", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("REST workspace status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var restStatus map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &restStatus); err != nil {
		t.Fatalf("Unmarshal REST status error = %v: %s", err, rr.Body.String())
	}
	if restStatus["state"] != "discrepancy" {
		t.Fatalf("REST workspace state = %#v, want discrepancy", restStatus["state"])
	}
	restSource := restStatus["sources"].([]any)[0].(map[string]any)
	if restSource["state"] != "branch-mismatch" || restSource["branch"] != workspaceName || restSource["current_ref"] != "codex/feature-a" {
		t.Fatalf("REST source status = %#v, want branch mismatch with branch/current_ref", restSource)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ workspaceStatus(name: "feature-a") { state sources { slot state branch currentRef unpushedReason } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL status errors = %#v", resp.Errors)
	}
	gqlStatus := resp.Data["workspaceStatus"].(map[string]any)
	gqlSource := gqlStatus["sources"].([]any)[0].(map[string]any)
	if gqlStatus["state"] != "discrepancy" || gqlSource["state"] != "branch-mismatch" || gqlSource["branch"] != workspaceName || gqlSource["currentRef"] != "codex/feature-a" {
		t.Fatalf("GraphQL status = %#v source = %#v, want same branch mismatch contract", gqlStatus, gqlSource)
	}

	resp = doGraphQL(t, server, map[string]any{
		"query": `{ gitOpsTopology { summary { branchMismatch unpushed } } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL topology errors = %#v", resp.Errors)
	}
	summary := resp.Data["gitOpsTopology"].(map[string]any)["summary"].(map[string]any)
	if summary["branchMismatch"] != float64(1) || summary["unpushed"] != float64(1) {
		t.Fatalf("GitOps summary = %#v, want one branch mismatch and unpushed worktree", summary)
	}

	runTestGit(t, workspaceSourcePath, "switch", workspaceName)
	writeTestFile(t, filepath.Join(cache, "main.txt"), "main update\n")
	runTestGit(t, cache, "add", "main.txt")
	runTestGit(t, cache, "commit", "-m", "main update")
	runTestGit(t, cache, "push", "fork", "main")

	resp = doGraphQL(t, server, map[string]any{
		"query": `mutation { workspaceSyncBase(name: "feature-a", method: "merge") { slot branch currentRef state } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL sync-base errors = %#v", resp.Errors)
	}
	synced := resp.Data["workspaceSyncBase"].([]any)[0].(map[string]any)
	if synced["slot"] != "app" || synced["branch"] != workspaceName || synced["currentRef"] != workspaceName || synced["state"] == "branch-mismatch" {
		t.Fatalf("workspaceSyncBase = %#v, want synced source still on workspace branch", synced)
	}
}

func TestRESTServiceErrorsUseTypedStatusCodes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  api:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/services/missing/start", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing service status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var notFound api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &notFound); err != nil {
		t.Fatalf("Unmarshal missing service error = %v", err)
	}
	if notFound.Kind != "service" || notFound.Name != "missing" {
		t.Fatalf("missing service error = %#v", notFound)
	}

	req = httptest.NewRequest(http.MethodPost, "/services", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid service status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var invalid api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &invalid); err != nil {
		t.Fatalf("Unmarshal invalid service error = %v", err)
	}
	if invalid.Field != "name" || invalid.Reason == "" {
		t.Fatalf("invalid service error = %#v", invalid)
	}
}

func TestRESTStackInitConflictUsesTypedStatusCode(t *testing.T) {
	root := t.TempDir()
	writeOperatorStackTemplate(t, root)
	writeTestFile(t, filepath.Join(root, ".angee", "existing.txt"), "present\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/stack/init", strings.NewReader(`{"template":"dev","yes":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stack init conflict status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var conflict api.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("Unmarshal stack init conflict = %v", err)
	}
	if conflict.Kind != "stack-root" || conflict.Name != filepath.Join(root, ".angee") || conflict.Reason == "" {
		t.Fatalf("stack init conflict = %#v", conflict)
	}
}

func TestGraphQLErrorsIncludeDomainExtensions(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  api:
    runtime: container
    image: nginx:latest
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `{ workspace(name: "missing") { name } }`,
	})
	if len(resp.Errors) != 1 {
		t.Fatalf("GraphQL errors = %#v, want one domain error", resp.Errors)
	}
	errObj, ok := resp.Errors[0].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL error = %#v, want object", resp.Errors[0])
	}
	extensions, ok := errObj["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("GraphQL error extensions = %#v, want object", errObj["extensions"])
	}
	if extensions["kind"] != "workspace" || extensions["name"] != "missing" {
		t.Fatalf("GraphQL error extensions = %#v, want workspace missing", extensions)
	}
}

func TestGraphQLServiceInit(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `mutation($input: ServiceInput!) {
			serviceInit(input: $input) { status name }
		}`,
		"variables": map[string]any{
			"input": map[string]any{
				"name":  "web",
				"image": "nginx:latest",
				"env": []map[string]string{
					{"key": "FOO", "value": "bar"},
				},
				"ports": []string{"8080:80"},
			},
		},
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	result := resp.Data["serviceInit"].(map[string]any)
	if result["status"] != "created" || result["name"] != "web" {
		t.Fatalf("serviceInit = %#v, want created web", result)
	}
	stack, err := manifest.LoadFile(manifest.Path(root))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	service, ok := stack.Services["web"]
	if !ok {
		t.Fatalf("service web was not persisted")
	}
	if service.Image != "nginx:latest" || service.Env["FOO"] != "bar" {
		t.Fatalf("service web = %#v, want image and env persisted", service)
	}
}

func TestGraphQLStackPrepareUsesBackendFieldNames(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
services:
  web:
    runtime: local
    command: ["go", "version"]
    workdir: /tmp
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	resp := doGraphQL(t, server, map[string]any{
		"query": `mutation { stackPrepare { processCompose } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	prepared := resp.Data["stackPrepare"].(map[string]any)
	processCompose := prepared["processCompose"].(map[string]any)
	processes := processCompose["processes"].(map[string]any)
	web := processes["web"].(map[string]any)
	if web["working_dir"] != "/tmp" {
		t.Fatalf("working_dir = %v, want /tmp; full process = %#v", web["working_dir"], web)
	}
	if _, exists := web["WorkingDir"]; exists {
		t.Fatalf("process exposes Go field name WorkingDir: %#v", web)
	}
}

func TestGraphQLRequiresPOST(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/graphql?query={health{status}}", nil)
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /graphql status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestGraphQLRejectsSimpleBrowserContentTypes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"mutation { stackDown { status } }"}`))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain GraphQL status = %d, want %d", rr.Code, http.StatusUnsupportedMediaType)
	}
}

func TestGraphQLRejectsCrossOriginBrowserPosts(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewBufferString(`{"query":"{ health { status } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin GraphQL status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestGraphQLBodySizeLimit(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := bytes.NewBuffer(make([]byte, maxGraphQLBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/graphql")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized GraphQL status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestGraphQLJSONBodySizeLimitIncludesTrailingBytes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := bytes.NewBufferString(`{"query":"{ health { status } }"}`)
	body.Write(bytes.Repeat([]byte(" "), maxGraphQLBodyBytes))
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON GraphQL status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
}

func writeTestStack(t *testing.T, root, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile(angee.yaml) error = %v", err)
	}
}

func writeTestFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func writeOperatorStackTemplate(t *testing.T, root string) {
	t.Helper()
	templateRoot := filepath.Join(root, ".templates", "stacks", "dev")
	manifestDir := filepath.Join(templateRoot, "template", "{{ ANGEE_ROOT }}")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(stack template) error = %v", err)
	}
	copierYAML := `_subdirectory: template
_templates_suffix: .jinja
_angee:
  kind: stack
  name: dev
ANGEE_ROOT:
  default: .angee
`
	writeTestFile(t, filepath.Join(templateRoot, "copier.yml"), copierYAML)
	manifestYAML := `version: 1
kind: stack
name: test
`
	writeTestFile(t, filepath.Join(manifestDir, "angee.yaml.jinja"), manifestYAML)
}

func setupOperatorGitWorkspace(t *testing.T) (string, string, string, string) {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	cache := filepath.Join(base, "cache")
	root := filepath.Join(base, ".angee")
	workspaceName := "feature-a"
	workspaceSourcePath := filepath.Join(root, "workspaces", workspaceName, "app")

	runTestGit(t, "", "init", "--bare", remote)
	runTestGit(t, "", "clone", remote, cache)
	runTestGit(t, cache, "remote", "rename", "origin", "fork")
	runTestGit(t, cache, "config", "user.email", "test@example.com")
	runTestGit(t, cache, "config", "user.name", "Test User")
	writeTestFile(t, filepath.Join(cache, "README.md"), "hello\n")
	runTestGit(t, cache, "add", "README.md")
	runTestGit(t, cache, "commit", "-m", "initial")
	runTestGit(t, cache, "branch", "-M", "main")
	runTestGit(t, cache, "push", "-u", "fork", "main")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root) error = %v", err)
	}
	runTestGit(t, cache, "worktree", "add", "-b", workspaceName, workspaceSourcePath, "main")
	runTestGit(t, workspaceSourcePath, "config", "user.email", "test@example.com")
	runTestGit(t, workspaceSourcePath, "config", "user.name", "Test User")

	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "test",
		Sources: map[string]manifest.Source{
			"app": {
				Kind:       "git",
				Repo:       remote,
				DefaultRef: "main",
				CachePath:  cache,
			},
		},
		Workspaces: map[string]manifest.Workspace{
			workspaceName: {
				Template: "workspaces/dev-pr",
				Sources: map[string]manifest.WorkspaceSource{
					"app": {
						Source:  "app",
						Mode:    "worktree",
						Branch:  workspaceName,
						Ref:     "main",
						Subpath: "app",
					},
				},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	return root, workspaceName, workspaceSourcePath, cache
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	gitArgs := append([]string{
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
	}, args...)
	cmd := exec.Command("git", gitArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

type graphQLTestResponse struct {
	Data   map[string]any `json:"data"`
	Errors []any          `json:"errors,omitempty"`
}

func doGraphQL(t *testing.T, server *Server, body map[string]any) graphQLTestResponse {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GraphQL HTTP status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp graphQLTestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal() error = %v: %s", err, rr.Body.String())
	}
	return resp
}
