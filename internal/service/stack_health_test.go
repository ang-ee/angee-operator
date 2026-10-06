package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

func exitCode(code int) *int { return &code }

// devFailureStack mirrors the stack from #86: deps (pnpm install) feeds codegen
// and storybook, codegen feeds frontend, and django and postgres stand alone.
func devFailureStack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "dev",
		Services: map[string]manifest.Service{
			"postgres":  {Runtime: manifest.RuntimeContainer, Image: "postgres:16"},
			"django":    {Runtime: manifest.RuntimeLocal, Command: []string{"serve"}},
			"frontend":  {Runtime: manifest.RuntimeLocal, Command: []string{"vite"}, DependsOn: []string{"codegen"}},
			"storybook": {Runtime: manifest.RuntimeLocal, Command: []string{"storybook"}, DependsOn: []string{"deps"}},
		},
		Jobs: map[string]manifest.Job{
			"deps":    {Runtime: manifest.RuntimeLocal, Command: []string{"pnpm", "install"}},
			"codegen": {Runtime: manifest.RuntimeLocal, Command: []string{"pnpm", "codegen"}, DependsOn: []string{"deps"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	for _, name := range []string{"docker-compose.yaml", "process-compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("generated\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	return root
}

func failedDepsStatuses() []runtime.ServiceStatus {
	return []runtime.ServiceStatus{
		{Name: "deps", Runtime: "local", State: "completed", ExitCode: exitCode(1)},
		{Name: "codegen", Runtime: "local", State: "skipped", ExitCode: exitCode(0)},
		{Name: "django", Runtime: "local", State: "running", ExitCode: exitCode(0)},
		{Name: "frontend", Runtime: "local", State: "skipped", ExitCode: exitCode(0)},
		{Name: "storybook", Runtime: "local", State: "skipped", ExitCode: exitCode(0)},
	}
}

func TestStackStatusReportsFailedJobAndSkippedDependents(t *testing.T) {
	root := devFailureStack(t)
	containers := stubStatusBackend{statuses: []runtime.ServiceStatus{{Name: "postgres", Runtime: "container", State: "running", ExitCode: exitCode(0)}}}
	platform, err := NewWithBackends(root, containers, stubStatusBackend{statuses: failedDepsStatuses()})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	status, err := platform.StackStatus(t.Context())
	if err != nil {
		t.Fatalf("StackStatus: %v", err)
	}
	if got := status.Jobs["deps"]; got.Status != api.JobFailed || got.ExitCode == nil || *got.ExitCode != 1 || got.Reason != "" {
		t.Fatalf("deps = %+v, want failed with exit 1", got)
	}
	const reason = "job deps failed (exit 1)"
	if got := status.Jobs["codegen"]; got.Status != api.JobSkipped || got.ExitCode != nil || got.Reason != reason {
		t.Fatalf("codegen = %+v, want skipped because %s", got, reason)
	}
	// frontend reaches deps through the skipped codegen; storybook directly.
	for _, name := range []string{"frontend", "storybook"} {
		if got := status.Services[name]; got.Status != "skipped" || got.Reason != reason {
			t.Fatalf("%s = %+v, want skipped because %s", name, got, reason)
		}
	}
	if got := status.Services["django"]; got.Status != "running" || got.Reason != "" {
		t.Fatalf("django = %+v, want running without a reason", got)
	}
	wantServices := api.StatusCounts{Total: 4, Running: 2, Skipped: 2}
	wantJobs := api.StatusCounts{Total: 2, Failed: 1, Skipped: 1}
	if status.Summary.Services != wantServices || status.Summary.Jobs != wantJobs {
		t.Fatalf("summary = %+v, want services %+v jobs %+v", status.Summary, wantServices, wantJobs)
	}
	failure := stackFailure(status)
	var stackErr *StackFailureError
	if !errors.As(failure, &stackErr) {
		t.Fatalf("stackFailure = %v, want *StackFailureError", failure)
	}
	want := "job \"deps\" failed (exit 1); 3 skipped: codegen, frontend, storybook; see `angee job logs deps`"
	if failure.Error() != want {
		t.Fatalf("failure = %q, want %q", failure.Error(), want)
	}
}

func TestStackStatusHealthyStackReportsNoFailure(t *testing.T) {
	root := devFailureStack(t)
	containers := stubStatusBackend{statuses: []runtime.ServiceStatus{{Name: "postgres", Runtime: "container", State: "running", ExitCode: exitCode(0)}}}
	local := stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "deps", State: "completed", ExitCode: exitCode(0)},
		{Name: "codegen", State: "completed", ExitCode: exitCode(0)},
		{Name: "django", State: "running", ExitCode: exitCode(0)},
		{Name: "frontend", State: "running", ExitCode: exitCode(0)},
		{Name: "storybook", State: "running", ExitCode: exitCode(0)},
	}}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	status, err := platform.StackStatus(t.Context())
	if err != nil {
		t.Fatalf("StackStatus: %v", err)
	}
	if got := status.Jobs["deps"]; got.Status != api.JobCompleted || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("deps = %+v, want completed with exit 0", got)
	}
	wantServices := api.StatusCounts{Total: 4, Running: 4}
	wantJobs := api.StatusCounts{Total: 2, Completed: 2}
	if status.Summary.Services != wantServices || status.Summary.Jobs != wantJobs {
		t.Fatalf("summary = %+v, want services %+v jobs %+v", status.Summary, wantServices, wantJobs)
	}
	if failure := stackFailure(status); failure != nil {
		t.Fatalf("stackFailure = %v, want nil for a healthy stack", failure)
	}
}

func TestStackStatusJobStates(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"migrate": {Runtime: manifest.RuntimeContainer, Image: "app"},
			"seed":    {Runtime: manifest.RuntimeContainer, Image: "app"},
			"fixture": {Runtime: manifest.RuntimeContainer, Image: "app"},
			"lint":    {Runtime: manifest.RuntimeLocal, Command: []string{"lint"}},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	for _, name := range []string{"docker-compose.yaml", "process-compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("generated\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	// A finished container job is only listed by `docker compose ps --all`;
	// the stopped supervisor leaves the local job without a run.
	containers := &statusRequestRecorder{stubStatusBackend: stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "migrate", State: "exited", ExitCode: exitCode(0)},
		{Name: "seed", State: "exited", ExitCode: exitCode(2)},
	}}}
	local := stubStatusBackend{err: errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")}
	platform, err := NewWithBackends(root, containers, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	jobs, _, err := platform.JobList(t.Context(), query.Args{})
	if err != nil {
		t.Fatalf("JobList: %v", err)
	}
	if len(containers.requests) != 1 || !containers.requests[0].All {
		t.Fatalf("compose status requests = %+v, want one with All", containers.requests)
	}
	got := map[string]api.JobState{}
	for _, job := range jobs {
		got[job.Name] = job
	}
	if job := got["migrate"]; job.Status != api.JobCompleted || job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("migrate = %+v, want completed (exit 0)", job)
	}
	if job := got["seed"]; job.Status != api.JobFailed || job.ExitCode == nil || *job.ExitCode != 2 {
		t.Fatalf("seed = %+v, want failed (exit 2)", job)
	}
	for _, name := range []string{"fixture", "lint"} {
		if job := got[name]; job.Status != api.JobNeverRun || job.ExitCode != nil || job.Reason != "" {
			t.Fatalf("%s = %+v, want never-run", name, job)
		}
	}
}

type statusRequestRecorder struct {
	stubStatusBackend
	requests []runtime.StatusRequest
}

func (b *statusRequestRecorder) Status(ctx context.Context, req runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	b.requests = append(b.requests, req)
	return b.stubStatusBackend.Status(ctx, req)
}

func TestRuntimeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		exit  *int
		job   bool
		want  string
	}{
		{"no record", "", nil, true, api.JobNeverRun},
		{"selective up leaves a job disabled", "Disabled", exitCode(0), true, api.JobNeverRun},
		{"waiting for a cron schedule", "Scheduled", exitCode(0), true, api.JobNeverRun},
		{"waiting on a dependency", "Pending", exitCode(0), true, api.JobPending},
		{"launching", "Launching", exitCode(0), true, api.JobRunning},
		{"job exit 0", "Completed", exitCode(0), true, api.JobCompleted},
		{"job exit 1", "Completed", exitCode(1), true, api.JobFailed},
		{"job killed by a signal still failed", "Completed", exitCode(-1), true, api.JobFailed},
		{"job command could not start", "Error", exitCode(0), true, api.JobFailed},
		{"job skipped", "Skipped", exitCode(0), true, api.JobSkipped},
		{"query failed", "unknown", nil, true, api.JobUnknown},
		{"service crashed", "Completed", exitCode(2), false, api.JobFailed},
		{"service stopped by signal", "Completed", exitCode(-1), false, serviceStopped},
		{"container stopped by SIGTERM", "exited", exitCode(143), false, serviceStopped},
		{"container killed after the grace period", "exited", exitCode(137), false, serviceStopped},
		{"container exited cleanly", "exited", exitCode(0), false, api.JobCompleted},
		{"container never started", "created", exitCode(0), false, serviceStopped},
		{"service running", "running", exitCode(0), false, api.JobRunning},
		{"service skipped", "Skipped", exitCode(0), false, api.JobSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeOutcome(runtime.ServiceStatus{State: tc.state, ExitCode: tc.exit}, tc.job); got != tc.want {
				t.Fatalf("runtimeOutcome(%q, exit %v, job=%v) = %q, want %q", tc.state, tc.exit, tc.job, got, tc.want)
			}
		})
	}
}

func TestSkipReasonNamesStartFailuresAndFallsBack(t *testing.T) {
	stack := &manifest.Stack{
		Services: map[string]manifest.Service{
			"api":    {Runtime: manifest.RuntimeLocal, DependsOn: []string{"assets", "migrate"}},
			"worker": {Runtime: manifest.RuntimeLocal, DependsOn: []string{"api"}},
			"admin":  {Runtime: manifest.RuntimeLocal, DependsOn: []string{"db"}},
			"db":     {Runtime: manifest.RuntimeLocal},
		},
		Jobs: map[string]manifest.Job{
			"assets":  {Runtime: manifest.RuntimeLocal},
			"migrate": {Runtime: manifest.RuntimeLocal},
		},
	}
	states := map[string]runtime.ServiceStatus{
		"assets":  {State: "Error"},
		"migrate": {State: "Completed", ExitCode: exitCode(3)},
		"api":     {State: "Skipped"},
		"worker":  {State: "Skipped"},
		"admin":   {State: "Skipped"},
		"db":      {State: "Running"},
	}
	outcomes := stackOutcomes(stack, states)
	if got, want := skipReason(stack, states, outcomes, "worker"), "job assets could not start; job migrate failed (exit 3)"; got != want {
		t.Fatalf("worker reason = %q, want %q", got, want)
	}
	if got, want := skipReason(stack, states, outcomes, "admin"), "a dependency failed or did not become ready"; got != want {
		t.Fatalf("admin reason = %q, want %q", got, want)
	}
}

// statusSequence is a StackStatusReader whose StackStatus returns each status
// in turn and then repeats the last one.
type statusSequence struct {
	mu       sync.Mutex
	statuses []api.StackStatusResponse
	calls    int
}

func (s *statusSequence) StackStatus(context.Context) (api.StackStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statuses[min(s.calls, len(s.statuses)-1)]
	s.calls++
	return status, nil
}

func jobsStatus(jobs ...api.JobState) api.StackStatusResponse {
	status := api.StackStatusResponse{Jobs: map[string]api.JobState{}, Services: map[string]api.ServiceState{}}
	for _, job := range jobs {
		status.Jobs[job.Name] = job
	}
	return status
}

func fastAwaitJobs(t *testing.T) {
	t.Helper()
	previous := awaitJobsInterval
	awaitJobsInterval = 5 * time.Millisecond
	t.Cleanup(func() { awaitJobsInterval = previous })
}

func TestCheckDevJobsWaitsForJobsAndReportsFailure(t *testing.T) {
	fastAwaitJobs(t)
	failed := jobsStatus(api.JobState{Name: "deps", Status: api.JobFailed, ExitCode: exitCode(1)}, api.JobState{Name: "codegen", Status: api.JobSkipped})
	failed.Services["frontend"] = api.ServiceState{Name: "frontend", Status: "skipped"}
	stack := &statusSequence{statuses: []api.StackStatusResponse{
		jobsStatus(api.JobState{Name: "deps", Status: api.JobRunning}, api.JobState{Name: "codegen", Status: api.JobPending}),
		jobsStatus(api.JobState{Name: "deps", Status: api.JobRunning}, api.JobState{Name: "codegen", Status: api.JobPending}),
		failed,
	}}
	var progress bytes.Buffer
	err := CheckDevJobs(t.Context(), stack, &progress)
	var failure *StackFailureError
	if !errors.As(err, &failure) {
		t.Fatalf("CheckDevJobs = %v, want *StackFailureError", err)
	}
	if want := "job \"deps\" failed (exit 1); 2 skipped: codegen, frontend; see `angee job logs deps`"; err.Error() != want {
		t.Fatalf("CheckDevJobs = %q, want %q", err.Error(), want)
	}
	if got, want := progress.String(), "waiting for jobs to finish: codegen, deps (Ctrl-C stops waiting; the stack keeps running)\n"; got != want {
		t.Fatalf("progress = %q, want one line %q", got, want)
	}
	if stack.calls != 3 {
		t.Fatalf("StackStatus calls = %d, want 3", stack.calls)
	}
}

func TestCheckDevJobsHealthyStackReturnsNil(t *testing.T) {
	fastAwaitJobs(t)
	stack := &statusSequence{statuses: []api.StackStatusResponse{
		jobsStatus(api.JobState{Name: "deps", Status: api.JobRunning}),
		jobsStatus(api.JobState{Name: "deps", Status: api.JobCompleted, ExitCode: exitCode(0)}),
	}}
	if err := CheckDevJobs(t.Context(), stack, io.Discard); err != nil {
		t.Fatalf("CheckDevJobs = %v, want nil for a healthy stack", err)
	}
	// A stack without jobs reads its status once and returns.
	empty := &statusSequence{statuses: []api.StackStatusResponse{jobsStatus()}}
	var progress bytes.Buffer
	if err := CheckDevJobs(t.Context(), empty, &progress); err != nil || empty.calls != 1 || progress.Len() != 0 {
		t.Fatalf("CheckDevJobs(no jobs) = %v after %d calls, progress %q; want nil after 1 call, silent", err, empty.calls, progress.String())
	}
}

func TestCheckDevJobsTimesOutNamingUnfinishedJobs(t *testing.T) {
	fastAwaitJobs(t)
	t.Setenv(jobRunTimeoutEnv, "30ms")
	running := jobsStatus(api.JobState{Name: "deps", Status: api.JobRunning}, api.JobState{Name: "lint", Status: api.JobFailed, ExitCode: exitCode(1)})
	stack := &statusSequence{statuses: []api.StackStatusResponse{running}}
	err := CheckDevJobs(t.Context(), stack, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "jobs not finished after 30ms: deps") {
		t.Fatalf("CheckDevJobs = %v, want a timeout naming deps", err)
	}
	var failure *StackFailureError
	if !errors.As(err, &failure) || !strings.Contains(err.Error(), `job "lint" failed (exit 1)`) {
		t.Fatalf("CheckDevJobs = %v, want the visible lint failure as well", err)
	}
}

// devJobsBackend is a local supervisor for StackDevForeground: it refuses
// status queries until its foreground run has started, which takes a moment as
// a real supervisor does, then reports statuses in turn.
type devJobsBackend struct {
	stubStatusBackend
	mu       sync.Mutex
	up       bool
	statuses [][]runtime.ServiceStatus
	calls    int
}

func (b *devJobsBackend) UpForeground(ctx context.Context, _ runtime.Target, _, _ io.Writer) error {
	time.Sleep(50 * time.Millisecond)
	b.mu.Lock()
	b.up = true
	b.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (b *devJobsBackend) Status(context.Context, runtime.StatusRequest) ([]runtime.ServiceStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.up {
		return nil, errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	}
	statuses := b.statuses[min(b.calls, len(b.statuses)-1)]
	b.calls++
	return statuses, nil
}

func runDevForeground(t *testing.T, local *devJobsBackend, settle func(*syncBuffer) bool) (string, error) {
	t.Helper()
	fastAwaitJobs(t)
	root := devFailureStack(t)
	platform, err := NewWithBackends(root, stubStatusBackend{}, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- platform.StackDevForeground(ctx, false, io.Discard, stderr) }()
	deadline := time.After(2 * time.Second)
	for !settle(stderr) {
		select {
		case err := <-done:
			t.Fatalf("StackDevForeground returned before Ctrl-C: %v", err)
		case <-deadline:
			t.Fatalf("dev did not settle; stderr = %q", stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Let the watch act on the status it read before Ctrl-C ends it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		return stderr.String(), err
	case <-time.After(2 * time.Second):
		t.Fatal("StackDevForeground did not return after cancellation")
	}
	return "", nil
}

func TestStackDevForegroundReportsFailedJobAndExitsNonZero(t *testing.T) {
	pending := []runtime.ServiceStatus{
		{Name: "deps", State: "Running", ExitCode: exitCode(0)},
		{Name: "codegen", State: "Pending", ExitCode: exitCode(0)},
		{Name: "django", State: "Running", ExitCode: exitCode(0)},
		{Name: "frontend", State: "Pending", ExitCode: exitCode(0)},
		{Name: "storybook", State: "Pending", ExitCode: exitCode(0)},
	}
	local := &devJobsBackend{statuses: [][]runtime.ServiceStatus{pending, pending, failedDepsStatuses()}}
	stderr, err := runDevForeground(t, local, func(b *syncBuffer) bool { return strings.Contains(b.String(), "angee dev:") })
	want := "job \"deps\" failed (exit 1); 3 skipped: codegen, frontend, storybook; see `angee job logs deps`"
	if got := strings.TrimSpace(stderr); got != "angee dev: "+want {
		t.Fatalf("stderr = %q, want the summary once jobs finish", stderr)
	}
	var failure *StackFailureError
	if !errors.As(err, &failure) || err.Error() != want {
		t.Fatalf("StackDevForeground = %v, want the summary as its error after Ctrl-C", err)
	}
}

func TestStackDevForegroundHealthyStackExitsZero(t *testing.T) {
	healthy := []runtime.ServiceStatus{
		{Name: "deps", State: "Completed", ExitCode: exitCode(0)},
		{Name: "codegen", State: "Completed", ExitCode: exitCode(0)},
		{Name: "django", State: "Running", ExitCode: exitCode(0)},
		{Name: "frontend", State: "Running", ExitCode: exitCode(0)},
		{Name: "storybook", State: "Running", ExitCode: exitCode(0)},
	}
	local := &devJobsBackend{statuses: [][]runtime.ServiceStatus{healthy}}
	stderr, err := runDevForeground(t, local, func(*syncBuffer) bool {
		local.mu.Lock()
		defer local.mu.Unlock()
		// The watch has read the settled jobs once the supervisor answered.
		return local.calls >= 2
	})
	if err != nil || stderr != "" {
		t.Fatalf("StackDevForeground = %v, stderr %q; want nil and silent for a healthy stack", err, stderr)
	}
}

// Ctrl-C while jobs are still running ends dev without a report: the runtime
// is shutting down, so its states no longer describe the bring-up.
func TestStackDevForegroundCtrlCBeforeJobsFinishExitsZero(t *testing.T) {
	running := []runtime.ServiceStatus{
		{Name: "deps", State: "Running", ExitCode: exitCode(0)},
		{Name: "codegen", State: "Pending", ExitCode: exitCode(0)},
		{Name: "django", State: "Running", ExitCode: exitCode(0)},
		{Name: "frontend", State: "Pending", ExitCode: exitCode(0)},
		{Name: "storybook", State: "Pending", ExitCode: exitCode(0)},
	}
	local := &devJobsBackend{statuses: [][]runtime.ServiceStatus{running}}
	stderr, err := runDevForeground(t, local, func(*syncBuffer) bool {
		local.mu.Lock()
		defer local.mu.Unlock()
		return local.calls >= 3
	})
	if err != nil || stderr != "" {
		t.Fatalf("StackDevForeground = %v, stderr %q; want nil and silent after Ctrl-C", err, stderr)
	}
}

// A dev stack without local processes has no supervisor to wait for; its
// container jobs are watched through the compose status alone.
func TestStackDevForegroundReportsFailedContainerJob(t *testing.T) {
	fastAwaitJobs(t)
	root := t.TempDir()
	stack := &manifest.Stack{
		Version:  manifest.VersionCurrent,
		Kind:     manifest.KindStack,
		Name:     "containers",
		Services: map[string]manifest.Service{"api": {Runtime: manifest.RuntimeContainer, Image: "app", DependsOn: []string{"migrate"}}},
		Jobs:     map[string]manifest.Job{"migrate": {Runtime: manifest.RuntimeContainer, Image: "app"}},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	containers := stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "migrate", State: "exited", ExitCode: exitCode(3)},
		{Name: "api", State: "created", ExitCode: exitCode(0)},
	}}
	platform, err := NewWithBackends(root, containers, stubStatusBackend{err: errors.New("process-compose must not be queried")})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- platform.StackDevForeground(ctx, false, io.Discard, stderr) }()
	want := "job \"migrate\" failed (exit 3); see `angee job logs migrate`"
	deadline := time.After(2 * time.Second)
	for !strings.Contains(stderr.String(), want) {
		select {
		case err := <-done:
			t.Fatalf("StackDevForeground returned before Ctrl-C: %v", err)
		case <-deadline:
			t.Fatalf("no report; stderr = %q", stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != want {
			t.Fatalf("StackDevForeground = %v, want %q", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StackDevForeground did not return after cancellation")
	}
}

// One failed status query must not end the wait: a job reported unknown is
// waited out for a few polls, and a runtime that stays unreachable ends it.
func TestCheckDevJobsWaitsOutUnknownJobs(t *testing.T) {
	fastAwaitJobs(t)
	unknown := jobsStatus(api.JobState{Name: "deps", Status: api.JobUnknown})
	stack := &statusSequence{statuses: []api.StackStatusResponse{
		unknown,
		jobsStatus(api.JobState{Name: "deps", Status: api.JobFailed, ExitCode: exitCode(1)}),
	}}
	var failure *StackFailureError
	if err := CheckDevJobs(t.Context(), stack, io.Discard); !errors.As(err, &failure) {
		t.Fatalf("CheckDevJobs = %v, want the failure read after the unknown poll", err)
	}
	unreachable := &statusSequence{statuses: []api.StackStatusResponse{unknown}}
	if err := CheckDevJobs(t.Context(), unreachable, io.Discard); err != nil || unreachable.calls != awaitJobsUnknownPolls {
		t.Fatalf("CheckDevJobs = %v after %d polls, want nil after %d", err, unreachable.calls, awaitJobsUnknownPolls)
	}
}

// Ctrl-C reaches the status queries the dev watch runs; their failure is not
// worth a warning once the context has ended.
func TestRuntimeServiceStatesSilentAfterCancellation(t *testing.T) {
	root := devFailureStack(t)
	platform, err := NewWithBackends(root, stubStatusBackend{err: errors.New("signal: interrupt")}, stubStatusBackend{err: errors.New("signal: interrupt")})
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelWarn))))
	cancel()
	// StackStatus returns early on an ended ctx; a query already in flight
	// when Ctrl-C lands reaches the merge with ctx ended, as here.
	platform.runtimeServiceStates(ctx, mustLoadStack(t, platform))
	if logs.Len() != 0 {
		t.Fatalf("logs = %q, want no warning after cancellation", logs.String())
	}
}

func mustLoadStack(t *testing.T, platform *Platform) *manifest.Stack {
	t.Helper()
	stack, err := platform.LoadStack()
	if err != nil {
		t.Fatalf("LoadStack: %v", err)
	}
	return stack
}

func TestCheckDevJobsCtrlCSaysTheStackKeepsRunning(t *testing.T) {
	fastAwaitJobs(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stack := &statusSequence{statuses: []api.StackStatusResponse{jobsStatus(api.JobState{Name: "deps", Status: api.JobRunning})}}
	err := CheckDevJobs(ctx, stack, io.Discard)
	if err == nil || err.Error() != "stopped waiting for jobs; the stack keeps running" {
		t.Fatalf("CheckDevJobs = %v, want the stopped-waiting message", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type logsRecorder struct {
	stubStatusBackend
	mu       sync.Mutex
	requests []runtime.LogsRequest
}

func (b *logsRecorder) Logs(_ context.Context, req runtime.LogsRequest) (<-chan string, error) {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	ch := make(chan string, 1)
	ch <- "deps | ERR_PNPM_ENOENT\n"
	close(ch)
	return ch, nil
}

func TestStackLogsAcceptsJobNames(t *testing.T) {
	root := devFailureStack(t)
	local := &logsRecorder{}
	platform, err := NewWithBackends(root, stubStatusBackend{}, local)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	lines, err := platform.StackLogs(t.Context(), []string{"deps"}, false)
	if err != nil {
		t.Fatalf("StackLogs(deps): %v", err)
	}
	var out strings.Builder
	for line := range lines {
		out.WriteString(line)
	}
	if out.String() != "deps | ERR_PNPM_ENOENT\n" || len(local.requests) != 1 || len(local.requests[0].Services) != 1 || local.requests[0].Services[0] != "deps" {
		t.Fatalf("logs = %q, requests %+v; want the deps process logs", out.String(), local.requests)
	}
	var notFound *NotFoundError
	if _, err := platform.StackLogs(t.Context(), []string{"missing"}, false); !errors.As(err, &notFound) {
		t.Fatalf("StackLogs(missing) = %v, want *NotFoundError", err)
	}
}
