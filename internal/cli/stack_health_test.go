package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
)

func exitCodePtr(code int) *int { return &code }

// failedDevStatus is the stack from #86 after deps failed: two jobs and four
// services, three of them skipped.
func failedDevStatus() api.StackStatusResponse {
	return api.StackStatusResponse{
		Name: "dev",
		Root: "/stack",
		Services: map[string]api.ServiceState{
			"django":    {Name: "django", Runtime: "local", Status: "running"},
			"postgres":  {Name: "postgres", Runtime: "container", Status: "running"},
			"frontend":  {Name: "frontend", Runtime: "local", Status: "skipped", Reason: "job deps failed (exit 1)"},
			"storybook": {Name: "storybook", Runtime: "local", Status: "skipped", Reason: "job deps failed (exit 1)"},
		},
		Jobs: map[string]api.JobState{
			"deps":    {Name: "deps", Runtime: "local", Status: api.JobFailed, ExitCode: exitCodePtr(1)},
			"codegen": {Name: "codegen", Runtime: "local", Status: api.JobSkipped, Reason: "job deps failed (exit 1)"},
		},
		Summary: api.StackSummary{
			Services: api.StatusCounts{Total: 4, Running: 2, Skipped: 2},
			Jobs:     api.StatusCounts{Total: 2, Failed: 1, Skipped: 1},
		},
	}
}

// fakeOperator serves /stack/status (and the list endpoints derived from it)
// from status, and accepts POST /stack/dev.
func fakeOperator(t *testing.T, status api.StackStatusResponse) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /stack/dev":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "started"})
		case "GET /stack/status":
			_ = json.NewEncoder(w).Encode(status)
		case "GET /jobs":
			resp := api.JobListResponse{}
			for _, name := range []string{"codegen", "deps", "lint", "seed"} {
				if job, ok := status.Jobs[name]; ok {
					resp.Nodes = append(resp.Nodes, job)
				}
			}
			resp.TotalCount = len(resp.Nodes)
			_ = json.NewEncoder(w).Encode(resp)
		case "GET /services":
			resp := api.ServiceListResponse{}
			for _, name := range []string{"django", "frontend", "postgres", "storybook"} {
				if svc, ok := status.Services[name]; ok {
					resp.Nodes = append(resp.Nodes, svc)
				}
			}
			resp.TotalCount = len(resp.Nodes)
			_ = json.NewEncoder(w).Encode(resp)
		case "GET /stack/logs":
			if got := r.URL.Query()["service"]; len(got) != 1 || got[0] != "deps" {
				t.Errorf("logs services = %v, want [deps]", got)
			}
			_, _ = w.Write([]byte("deps | ERR_PNPM_ENOENT\n"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestStatusPrintsOutcomeCounts(t *testing.T) {
	server := fakeOperator(t, failedDevStatus())
	stdout, _, err := runCLI(t, "--operator", server.URL, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	want := "dev\nroot: /stack\nservices: 4 (2 running, 2 skipped)\njobs: 2 (1 failed, 1 skipped)\nworkspaces: 0\n"
	if stdout != want {
		t.Fatalf("status output = %q, want %q", stdout, want)
	}
	stdout, _, err = runCLI(t, "--operator", server.URL, "--json", "status")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	if !strings.Contains(stdout, `"summary": {`) || !strings.Contains(stdout, `"failed": 1`) || !strings.Contains(stdout, `"skipped": 2`) {
		t.Fatalf("status --json = %s, want the summary counts", stdout)
	}
}

func TestStatusWithoutObservedOutcomesPrintsBareCounts(t *testing.T) {
	status := api.StackStatusResponse{
		Name:     "down",
		Root:     "/stack",
		Services: map[string]api.ServiceState{"web": {Name: "web", Runtime: "local", Status: "declared"}},
		Jobs:     map[string]api.JobState{"deps": {Name: "deps", Runtime: "local", Status: api.JobNeverRun}},
	}
	server := fakeOperator(t, status)
	stdout, _, err := runCLI(t, "--operator", server.URL, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if want := "down\nroot: /stack\nservices: 1\njobs: 1\nworkspaces: 0\n"; stdout != want {
		t.Fatalf("status output = %q, want %q", stdout, want)
	}
}

func TestJobListShowsLastRun(t *testing.T) {
	status := failedDevStatus()
	status.Jobs["lint"] = api.JobState{Name: "lint", Runtime: "local", Status: api.JobCompleted, ExitCode: exitCodePtr(0)}
	status.Jobs["seed"] = api.JobState{Name: "seed", Runtime: "container", Status: api.JobNeverRun}
	server := fakeOperator(t, status)
	stdout, _, err := runCLI(t, "--operator", server.URL, "job", "list")
	if err != nil {
		t.Fatalf("job list: %v", err)
	}
	want := "codegen\tlocal\tskipped\tjob deps failed (exit 1)\n" +
		"deps\tlocal\tfailed (exit 1)\n" +
		"lint\tlocal\tcompleted (exit 0)\n" +
		"seed\tcontainer\tnever-run\n"
	if stdout != want {
		t.Fatalf("job list = %q, want %q", stdout, want)
	}
	stdout, _, err = runCLI(t, "--operator", server.URL, "--json", "job", "list")
	if err != nil {
		t.Fatalf("job list --json: %v", err)
	}
	var jobs []api.JobState
	if err := json.Unmarshal([]byte(stdout), &jobs); err != nil {
		t.Fatalf("job list --json = %s: %v", stdout, err)
	}
	if len(jobs) != 4 || jobs[1].Status != api.JobFailed || jobs[1].ExitCode == nil || *jobs[1].ExitCode != 1 || jobs[3].Status != api.JobNeverRun {
		t.Fatalf("job list --json = %+v", jobs)
	}
}

func TestServiceListShowsSkipReason(t *testing.T) {
	server := fakeOperator(t, failedDevStatus())
	stdout, _, err := runCLI(t, "--operator", server.URL, "service", "list")
	if err != nil {
		t.Fatalf("service list: %v", err)
	}
	want := "django\tlocal\trunning\n" +
		"frontend\tlocal\tskipped\tjob deps failed (exit 1)\n" +
		"postgres\tcontainer\trunning\n" +
		"storybook\tlocal\tskipped\tjob deps failed (exit 1)\n"
	if stdout != want {
		t.Fatalf("service list = %q, want %q", stdout, want)
	}
}

func TestDevDetachReportsFailedJobAndExitsNonZero(t *testing.T) {
	server := fakeOperator(t, failedDevStatus())
	stdout, _, err := runCLI(t, "--operator", server.URL, "dev", "-d")
	if stdout != "dev stack started in background\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	var failure *service.StackFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("dev -d error = %v, want *service.StackFailureError", err)
	}
	want := "job \"deps\" failed (exit 1); 3 skipped: codegen, frontend, storybook; see `angee job logs deps`"
	if err.Error() != want {
		t.Fatalf("dev -d error = %q, want %q", err.Error(), want)
	}
	if ExitCode(err) == 0 {
		t.Fatal("dev -d exit code = 0, want non-zero")
	}
}

func TestDevDetachHealthyStackExitsZero(t *testing.T) {
	status := failedDevStatus()
	status.Jobs["deps"] = api.JobState{Name: "deps", Runtime: "local", Status: api.JobCompleted, ExitCode: exitCodePtr(0)}
	status.Jobs["codegen"] = api.JobState{Name: "codegen", Runtime: "local", Status: api.JobCompleted, ExitCode: exitCodePtr(0)}
	for _, name := range []string{"frontend", "storybook"} {
		status.Services[name] = api.ServiceState{Name: name, Runtime: "local", Status: "running"}
	}
	server := fakeOperator(t, status)
	stdout, stderr, err := runCLI(t, "--operator", server.URL, "dev", "-d")
	if err != nil || stdout != "dev stack started in background\n" || stderr != "" {
		t.Fatalf("dev -d = %v, stdout %q, stderr %q; want success", err, stdout, stderr)
	}
}

func TestJobLogsShowsJobOutput(t *testing.T) {
	server := fakeOperator(t, failedDevStatus())
	stdout, _, err := runCLI(t, "--operator", server.URL, "job", "logs", "deps")
	if err != nil {
		t.Fatalf("job logs: %v", err)
	}
	if stdout != "deps | ERR_PNPM_ENOENT\n" {
		t.Fatalf("job logs = %q", stdout)
	}
}
