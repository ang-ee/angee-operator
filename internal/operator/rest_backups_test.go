package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
)

func TestBackupRoutesRequireConfiguredAdminBearer(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "backup-admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	minted, err := server.tokens.MintConnection("browser-user", []string{"*", "backup", "admin"}, "1h")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{http.MethodPost, "/backups/preview"}, {http.MethodPost, "/backups"}, {http.MethodGet, "/backups/example/archive"}} {
		for _, credential := range []struct {
			value  string
			status int
		}{{"", http.StatusUnauthorized}, {"Bearer bad", http.StatusUnauthorized}, {"Bearer " + minted.Token, http.StatusForbidden}} {
			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{"version":1,"selection":{"version":1}}`))
			req.Header.Set("Authorization", credential.value)
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, req)
			if response.Code != credential.status {
				t.Fatalf("%s %s status=%d expected=%d", route.method, route.path, response.Code, credential.status)
			}
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/backups/preview", bytes.NewBufferString(`{"version":1,"selection":{"version":1}}`))
	req.Header.Set("Authorization", "Bearer backup-admin")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("admin preview=%d %s", response.Code, response.Body.String())
	}
	// Strict transport decoding catches a stale client spelling before any capture.
	req = httptest.NewRequest(http.MethodPost, "/backups/preview", bytes.NewBufferString(`{"version":1,"selection":{"services":{"mode":"all"}}}`))
	req.Header.Set("Authorization", "Bearer backup-admin")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d %s", response.Code, response.Body.String())
	}
}

// backupDecodeAPI records whether a backup request reached the platform.
type backupDecodeAPI struct {
	service.API
	called bool
}

func (f *backupDecodeAPI) BackupPreview(context.Context, api.BackupRequest) (api.BackupPreview, error) {
	f.called = true
	return api.BackupPreview{Version: 1}, nil
}

func (f *backupDecodeAPI) BackupStart(_ context.Context, req api.BackupRequest) (api.JobRunOperation, error) {
	f.called = true
	return api.JobRunOperation{ID: req.RequestID}, nil
}

// Backup bodies are decoded strictly, so a misspelled selection field or a
// second value is a 400 before discovery or capture, while every other REST
// body keeps the lenient decoding it always had.
func TestBackupBodiesDecodeStrictlyAndOtherBodiesLeniently(t *testing.T) {
	for _, body := range []string{
		`{"version":1,"request_id":"r1","selection":{"version":1},"selecton":{"storage":{"mode":"all"}}}`,
		`{"version":1,"request_id":"r1","selection":{"version":1,"services":{"mode":"all"}}}`,
		`{"version":1,"request_id":"r1","selection":{"version":1,"storage":{"mode":"all","name":["data"]}}}`,
		`{"version":1,"request_id":"r1","selection":{"version":1}} {"version":1}`,
		`{"version":1,"request_id":"r1","selection":{"version":1}} trailing`,
	} {
		for _, route := range []struct {
			path    string
			handler func(*Server) http.HandlerFunc
		}{{"/backups/preview", func(s *Server) http.HandlerFunc { return s.backupPreview }}, {"/backups", func(s *Server) http.HandlerFunc { return s.backupStart }}} {
			fake := &backupDecodeAPI{}
			res := httptest.NewRecorder()
			route.handler(&Server{platform: fake})(res, httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body)))
			var got api.ErrorResponse
			_ = json.Unmarshal(res.Body.Bytes(), &got)
			if res.Code != http.StatusBadRequest || got.Code != service.CodeInvalidInput || fake.called {
				t.Fatalf("POST %s %s = %d %s (platform called: %v), want 400 INVALID_INPUT before the platform", route.path, body, res.Code, res.Body.String(), fake.called)
			}
		}
	}
	fake := &backupDecodeAPI{}
	res := httptest.NewRecorder()
	(&Server{platform: fake}).backupStart(res, httptest.NewRequest(http.MethodPost, "/backups", strings.NewReader(`{"version":1,"request_id":"r1","selection":{"version":1}}`)))
	if res.Code != http.StatusAccepted || !fake.called {
		t.Fatalf("POST /backups = %d %s, want an exact request accepted", res.Code, res.Body.String())
	}

	for _, body := range []string{
		`{"inputs":{"target":"web"},"chained_restart":true,"unknown":"ignored"}`,
		`{"inputs":{"target":"web"},"chained_restart":true} {"ignored":true}`,
	} {
		fake := &jobRunAPI{startedOperation: api.JobRunOperation{ID: "run-1", RootJob: "codegen", Status: api.JobRunPending}}
		req := httptest.NewRequest(http.MethodPost, "/jobs/codegen/run", strings.NewReader(body))
		req.SetPathValue("name", "codegen")
		res := httptest.NewRecorder()
		(&Server{platform: fake}).jobRun(res, req)
		if res.Code != http.StatusAccepted || fake.inputs["target"] != "web" || !fake.chainedRestart {
			t.Fatalf("POST /jobs/codegen/run %s = %d %s, want the lenient body accepted", body, res.Code, res.Body.String())
		}
	}
}

// A capture job is refused on the job routes and the GraphQL jobRun and
// jobRunPreview fields, with or without inputs, as INVALID_INPUT naming it.
func TestCaptureJobRefusedOnJobRoutes(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, `version: 1
kind: stack
name: test
jobs:
  dump:
    runtime: local
    command: [trusted-dump]
    capture: {group: databases, name: app, output: app.dump}
`)
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/jobs/dump/run", ""},
		{http.MethodPost, "/jobs/dump/run", `{"inputs":{"backup_output":"../outside"}}`},
		{http.MethodPost, "/jobs/dump/run", `{"inputs":{"backup_output":"../outside"},"chained_restart":true}`},
		{http.MethodGet, "/jobs/dump/run-preview", ""},
	} {
		res := serveREST(server, request.method, request.path, request.body)
		var got api.ErrorResponse
		if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s %s: %v: %s", request.method, request.path, err, res.Body.String())
		}
		if res.Code != http.StatusBadRequest || got.Code != service.CodeInvalidInput || got.Job != "dump" || got.Hint == "" {
			t.Fatalf("%s %s %s = %d %+v, want 400 INVALID_INPUT naming job dump", request.method, request.path, request.body, res.Code, got)
		}
	}
	for _, query := range []string{
		`mutation { jobRun(name: "dump") { id } }`,
		`mutation { jobRun(name: "dump", inputs: [{key: "backup_output", value: "../outside"}]) { id } }`,
		`{ jobRunPreview(name: "dump", chainedRestart: false) { jobs } }`,
	} {
		res := serveRequest(server, graphQLRequest(t, query))
		var resp struct {
			Errors []struct {
				Extensions map[string]any `json:"extensions"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &resp); err != nil || len(resp.Errors) != 1 {
			t.Fatalf("%s = %s (%v), want one error", query, res.Body.String(), err)
		}
		if ext := resp.Errors[0].Extensions; ext["code"] != service.CodeInvalidInput || ext["job"] != "dump" {
			t.Fatalf("%s extensions = %#v, want INVALID_INPUT naming job dump", query, ext)
		}
	}
	if resp := doGraphQL(t, server, map[string]any{"query": `{ latestJobRun { id } }`}); len(resp.Errors) != 0 || resp.Data["latestJobRun"] != nil {
		t.Fatalf("latestJobRun = %#v, want null after refusals", resp)
	}
}

// A backup's receipt is served by GET /job-runs/{id} and GraphQL
// jobRunOperation with node kinds the schema declares, and never as the latest
// job run.
func TestBackupReceiptReadableOverRESTAndGraphQL(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	writeTestFile(t, filepath.Join(root, ".env"), "SECRET_KEY=original-recovery-key\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "backup-admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	authorized := func(req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("Authorization", "Bearer backup-admin")
		return serveRequest(server, req)
	}
	if res := authorized(httptestRequest(http.MethodPost, "/backups", `{"version":1,"request_id":"nightly-1","selection":{"version":1}}`)); res.Code != http.StatusAccepted {
		t.Fatalf("POST /backups = %d %s", res.Code, res.Body.String())
	}

	query := `{ jobRunOperation(id: "nightly-1") { id rootJob status nodes { name kind status } } latestJobRun { id } __type(name: "JobRunNodeKind") { enumValues { name } } }`
	var resp struct {
		Data struct {
			JobRunOperation *struct {
				ID      string `json:"id"`
				RootJob string `json:"rootJob"`
				Status  string `json:"status"`
				Nodes   []struct {
					Name   string `json:"name"`
					Kind   string `json:"kind"`
					Status string `json:"status"`
				} `json:"nodes"`
			} `json:"jobRunOperation"`
			LatestJobRun *struct{ ID string } `json:"latestJobRun"`
			Type         struct {
				EnumValues []struct{ Name string } `json:"enumValues"`
			} `json:"__type"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := authorized(graphQLRequest(t, query))
		resp.Data.JobRunOperation = nil
		if err := json.Unmarshal(res.Body.Bytes(), &resp); err != nil || len(resp.Errors) != 0 || resp.Data.JobRunOperation == nil {
			t.Fatalf("GraphQL jobRunOperation = %s (%v), want the backup receipt without errors", res.Body.String(), err)
		}
		if status := resp.Data.JobRunOperation.Status; status != "PENDING" && status != "RUNNING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup did not finish: %s", res.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	declared := map[string]bool{}
	for _, value := range resp.Data.Type.EnumValues {
		declared[value.Name] = true
	}
	op := resp.Data.JobRunOperation
	if op.ID != "nightly-1" || op.RootJob != "backup" || op.Status != "SUCCEEDED" || len(op.Nodes) != 2 {
		t.Fatalf("jobRunOperation = %+v, want the succeeded backup receipt", op)
	}
	for _, node := range op.Nodes {
		if node.Kind != "BACKUP" || !declared[node.Kind] {
			t.Fatalf("node %+v, want kind BACKUP declared by JobRunNodeKind %v", node, resp.Data.Type.EnumValues)
		}
	}
	if resp.Data.LatestJobRun != nil {
		t.Fatalf("latestJobRun = %+v, want null: a backup is not a job run", resp.Data.LatestJobRun)
	}

	res := authorized(httptestRequest(http.MethodGet, "/job-runs/nightly-1", ""))
	var receipt api.JobRunOperation
	if err := json.Unmarshal(res.Body.Bytes(), &receipt); err != nil || res.Code != http.StatusOK || receipt.ID != "nightly-1" || receipt.Artifact == nil {
		t.Fatalf("GET /job-runs/nightly-1 = %d %s (%v), want the receipt with its artifact", res.Code, res.Body.String(), err)
	}
	if res := authorized(httptestRequest(http.MethodGet, "/job-runs/latest", "")); res.Code != http.StatusOK || strings.TrimSpace(res.Body.String()) != "null" {
		t.Fatalf("GET /job-runs/latest = %d %s, want null", res.Code, res.Body.String())
	}
}

// A backup's receipt names every archived path and the digest of the captured
// secrets, so only the admin bearer that /backups requires sees it: to a
// minted token it does not exist, over REST and GraphQL alike. A job that is
// merely named backup is no backup.
func TestBackupReceiptHiddenFromMintedTokens(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	writeTestFile(t, filepath.Join(root, ".env"), "SECRET_KEY=original-recovery-key\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "backup-admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	minted, err := server.tokens.MintConnection("alice", []string{"*"}, "1h")
	if err != nil {
		t.Fatal(err)
	}
	as := func(token string, req *http.Request) *httptest.ResponseRecorder {
		req.Header.Set("Authorization", "Bearer "+token)
		return serveRequest(server, req)
	}
	if res := as("backup-admin", httptestRequest(http.MethodPost, "/backups", `{"version":1,"request_id":"nightly-1","selection":{"version":1}}`)); res.Code != http.StatusAccepted {
		t.Fatalf("POST /backups = %d %s", res.Code, res.Body.String())
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var receipt api.JobRunOperation
		res := as("backup-admin", httptestRequest(http.MethodGet, "/job-runs/nightly-1", ""))
		if err := json.Unmarshal(res.Body.Bytes(), &receipt); err != nil || res.Code != http.StatusOK {
			t.Fatalf("admin GET /job-runs/nightly-1 = %d %s (%v)", res.Code, res.Body.String(), err)
		}
		if receipt.Artifact != nil {
			break
		}
		if time.Now().After(deadline) || (receipt.Status != api.JobRunPending && receipt.Status != api.JobRunRunning) {
			t.Fatalf("admin GET /job-runs/nightly-1 = %s, want a completed backup with its artifact", res.Body.String())
		}
	}

	res := as(minted.Token, httptestRequest(http.MethodGet, "/job-runs/nightly-1", ""))
	if res.Code != http.StatusNotFound || strings.Contains(res.Body.String(), "secrets.env") {
		t.Fatalf("minted GET /job-runs/nightly-1 = %d %s, want 404 without the receipt", res.Code, res.Body.String())
	}
	if unknown := as(minted.Token, httptestRequest(http.MethodGet, "/job-runs/no-such-run", "")); unknown.Code != res.Code {
		t.Fatalf("minted GET of an unknown run = %d, want the same %d as a hidden backup", unknown.Code, res.Code)
	}
	var resp struct {
		Data struct {
			JobRunOperation *struct{ ID string } `json:"jobRunOperation"`
		} `json:"data"`
		Errors []struct {
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	gql := as(minted.Token, graphQLRequest(t, `{ jobRunOperation(id: "nightly-1") { id } }`))
	if err := json.Unmarshal(gql.Body.Bytes(), &resp); err != nil || resp.Data.JobRunOperation != nil || len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != service.CodeNotFound {
		t.Fatalf("minted GraphQL jobRunOperation = %s (%v), want NOT_FOUND and no receipt", gql.Body.String(), err)
	}

	job := api.JobRunOperation{ID: "run-1", RootJob: "backup", Nodes: []api.JobRunNode{{Name: "backup", Kind: "job"}}}
	if service.IsBackupReceipt(job) {
		t.Fatal("IsBackupReceipt() = true for a run of a job named backup")
	}
}
