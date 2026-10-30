package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	opgql "github.com/ang-ee/angee-operator/internal/operator/gql"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
	"github.com/ang-ee/angee-operator/internal/service"
)

// logsAPI answers stack status with a crashed service and records the tail of
// each logs read.
type logsAPI struct {
	service.API
	tails []int
}

func (f *logsAPI) StackStatus(context.Context) (api.StackStatusResponse, error) {
	exit := 1
	return api.StackStatusResponse{
		Name: "dev",
		Services: map[string]api.ServiceState{
			"celery-worker": {Name: "celery-worker", Runtime: "local", Status: "Completed", Outcome: api.JobFailed, ExitCode: &exit},
		},
		Summary: api.StackSummary{Services: api.StatusCounts{Total: 1, Failed: 1}},
	}, nil
}

func (f *logsAPI) StackLogs(ctx context.Context, services []string, follow bool, tail int) (<-chan string, error) {
	return f.StackLogsLimited(ctx, services, follow, tail, 0)
}

func (f *logsAPI) StackLogsLimited(_ context.Context, _ []string, _ bool, tail, _ int) (<-chan string, error) {
	f.tails = append(f.tails, tail)
	ch := make(chan string, 1)
	ch <- "line\n"
	close(ch)
	return ch, nil
}

func fakeGraphQL(t *testing.T, platform service.API, query string) map[string]any {
	t.Helper()
	handler, _, err := newGraphQLHandler(&Server{platform: platform})
	if err != nil {
		t.Fatalf("newGraphQLHandler() error = %v", err)
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	var resp struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", rr.Body.String(), err)
	}
	if len(resp.Errors) > 0 {
		t.Fatalf("GraphQL errors = %#v", resp.Errors)
	}
	return resp.Data
}

// A crashed service is served as failed with its exit code beside the raw
// runtime status (#112).
func TestGraphQLServesAFailedServiceOutcomeAndExitCode(t *testing.T) {
	data := fakeGraphQL(t, &logsAPI{}, `{ stackStatus { services { name status outcome exitCode } } }`)
	service := data["stackStatus"].(map[string]any)["services"].([]any)[0].(map[string]any)
	if service["name"] != "celery-worker" || service["status"] != "Completed" || service["outcome"] != api.JobFailed || service["exitCode"] != float64(1) {
		t.Fatalf("service = %#v, want celery-worker Completed, outcome failed, exitCode 1", service)
	}
}

// The GraphQL logs fields pass their tail to the platform, 0 when absent.
func TestGraphQLLogsFieldsPassTheTail(t *testing.T) {
	platform := &logsAPI{}
	// One query each: gqlgen resolves a query's root fields concurrently.
	for _, query := range []string{
		`{ stackLogs(services: ["web"], tail: 50) }`,
		`{ serviceLogs(name: "web", tail: 7) }`,
		`{ serviceLogs(name: "web") }`,
	} {
		fakeGraphQL(t, platform, query)
	}
	if got, want := platform.tails, []int{50, 7, 0}; !slices.Equal(got, want) {
		t.Fatalf("tails = %v, want %v", got, want)
	}
}

// `stackLogs(services: [api, worker], limit: 60)` returns the newest whole,
// labelled lines of the read, behind one marker, within 60 bytes (#118). It
// used to return `api    | api-1495 … api   \n[truncated]`: cut mid-line, with
// nothing from worker. This runs the platform and the process-compose backend
// against a fake supervisor's log range endpoint.
func TestGraphQLStackLogsKeepsTheNewestWholeLines(t *testing.T) {
	api := make([]string, 1500)
	for i := range api {
		api[i] = "api-" + strconv.Itoa(i)
	}
	lines := map[string][]string{"api": api, "worker": {"worker-1"}}
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/process/logs/"), "/")
		process := lines[name]
		if limit, _ := strconv.Atoi(path.Base(r.URL.Path)); limit > 0 && limit < len(process) {
			process = process[len(process)-limit:]
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"logs": process})
	}))
	t.Cleanup(supervisor.Close)
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "logs",
		Ports:   map[string]manifest.Port{"process_compose": {Value: supervisor.Listener.Addr().(*net.TCPAddr).Port}},
		Services: map[string]manifest.Service{
			"api":    {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
			"worker": {Runtime: manifest.RuntimeLocal, Command: []string{"work"}},
		},
	}); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "process-compose.yaml"), []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform, err := service.NewWithBackends(root, nil, proccompose.Backend{})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	data := fakeGraphQL(t, platform, `{ stackLogs(services: ["api", "worker"], limit: 60) }`)
	if got, want := data["stackLogs"], "[truncated]\napi    | api-1499\nworker | worker-1\n"; got != want {
		t.Fatalf("stackLogs = %q, want %q", got, want)
	}
}

// onServiceLogs follows with its tail, through the event hub.
func TestOnServiceLogsPassesTheTail(t *testing.T) {
	platform := &logsAPI{}
	hub := opgql.NewEventHub(platform, slog.New(slog.DiscardHandler))
	lines, err := hub.SubscribeServiceLogs(t.Context(), "web", 30)
	if err != nil {
		t.Fatalf("SubscribeServiceLogs() error = %v", err)
	}
	for range lines {
	}
	if got, want := platform.tails, []int{30}; !slices.Equal(got, want) {
		t.Fatalf("tails = %v, want %v", got, want)
	}
}

// The REST logs endpoints pass `?tail=` to the platform and refuse one that is
// not a number.
func TestRESTLogsPassTheTail(t *testing.T) {
	platform := &logsAPI{}
	server := &Server{platform: platform}
	for _, tt := range []struct {
		target string
		serve  func(http.ResponseWriter, *http.Request)
	}{
		{"/stack/logs?service=web&tail=50", server.stackLogs},
		{"/services/web/logs?tail=7", server.serviceLogs},
		{"/services/web/logs", server.serviceLogs},
	} {
		req := httptest.NewRequest(http.MethodGet, tt.target, nil)
		req.SetPathValue("name", "web")
		rr := httptest.NewRecorder()
		tt.serve(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s, want 200", tt.target, rr.Code, rr.Body.String())
		}
	}
	if got, want := platform.tails, []int{50, 7, 0}; !slices.Equal(got, want) {
		t.Fatalf("tails = %v, want %v", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/stack/logs?tail=lots", nil)
	rr := httptest.NewRecorder()
	server.stackLogs(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "tail") {
		t.Fatalf("GET /stack/logs?tail=lots = %d %s, want 400 naming tail", rr.Code, rr.Body.String())
	}
}
