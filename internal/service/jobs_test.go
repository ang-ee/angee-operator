package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

type recordingJobBackend struct {
	stubStatusBackend
	mu          sync.Mutex
	calls       []string
	failJobs    map[string]error
	failStops   map[string]error
	failApply   map[string]error
	beforeJob   func(context.Context, runtime.JobSpec)
	beforeApply func()
}

func (b *recordingJobBackend) RunJob(ctx context.Context, _ runtime.Target, job runtime.JobSpec) ([]byte, error) {
	if b.beforeJob != nil {
		b.beforeJob(ctx, job)
	}
	b.mu.Lock()
	b.calls = append(b.calls, "job:"+job.Name)
	err := b.failJobs[job.Name]
	b.mu.Unlock()
	return []byte(job.Name + " output"), err
}

func (b *recordingJobBackend) Apply(_ context.Context, target runtime.Target) error {
	if b.beforeApply != nil {
		b.beforeApply()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "service:"+target.Services[0])
	return b.failApply[target.Services[0]]
}

func (b *recordingJobBackend) Stop(ctx context.Context, target runtime.Target) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "stop:"+target.Services[0])
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.failStops[target.Services[0]]
}

func (b *recordingJobBackend) recordedCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func newJobOperationPlatform(t *testing.T, stack *manifest.Stack, backend *recordingJobBackend, root string, chained bool) (*Platform, string) {
	t.Helper()
	stackRoot := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(stackRoot), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackRoot, "process-compose.yaml"), []byte("version: 0.5\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(process-compose.yaml): %v", err)
	}
	p, err := NewWithBackends(stackRoot, backend, backend)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	nodes, err := jobRunNodes(stack, root, chained)
	if err != nil {
		t.Fatalf("jobRunNodes: %v", err)
	}
	const id = "test-operation"
	compiled, err := Compile(stack, stackRoot, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	p.jobRuns[id] = api.JobRunOperation{ID: id, RootJob: root, ChainedRestart: chained, Status: api.JobRunPending, StartedAt: time.Now(), Nodes: nodes}
	p.jobRunStacks[id] = stack
	p.jobRunCompiled[id] = compiled
	return p, id
}

func TestExecuteJobRunOrdersChainedJoinOnce(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"root":  {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
			"left":  {Runtime: manifest.RuntimeLocal, Command: []string{"left"}, DependsOn: []string{"root"}},
			"right": {Runtime: manifest.RuntimeLocal, Command: []string{"right"}, DependsOn: []string{"root"}},
		},
		Services: map[string]manifest.Service{
			"join": {Runtime: manifest.RuntimeLocal, Command: []string{"join"}, DependsOn: []string{"left", "right"}},
		},
	}
	zero := 0
	backend := &recordingJobBackend{stubStatusBackend: stubStatusBackend{statuses: []runtime.ServiceStatus{{Name: "join", State: "running", ExitCode: &zero}}}}
	p, id := newJobOperationPlatform(t, stack, backend, "root", true)

	p.executeJobRun(context.Background(), id, func() {})

	op, err := p.JobRunGet(context.Background(), id)
	if err != nil {
		t.Fatalf("JobRunGet: %v", err)
	}
	if op.Status != api.JobRunSucceeded || op.EndedAt == nil {
		t.Fatalf("operation = %#v, want completed success", op)
	}
	if got, want := backend.recordedCalls(), []string{"job:root", "job:left", "job:right", "service:join"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for _, node := range op.Nodes {
		if node.Status != api.JobRunSucceeded {
			t.Errorf("node %q status = %q, want succeeded", node.Name, node.Status)
		}
	}
	// Returned receipts are snapshots: a caller cannot mutate daemon-owned state.
	op.Nodes[0].Status = api.JobRunFailed
	again, err := p.JobRunGet(context.Background(), id)
	if err != nil {
		t.Fatalf("second JobRunGet: %v", err)
	}
	if again.Nodes[0].Status != api.JobRunSucceeded {
		t.Fatalf("stored node status changed through receipt slice: %q", again.Nodes[0].Status)
	}
}

func TestExecuteJobRunBlocksAllDescendantsAfterRootFailure(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"root":  {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
			"child": {Runtime: manifest.RuntimeLocal, Command: []string{"child"}, DependsOn: []string{"root"}},
		},
		Services: map[string]manifest.Service{
			"web": {Runtime: manifest.RuntimeLocal, Command: []string{"web"}, DependsOn: []string{"child"}},
		},
	}
	backend := &recordingJobBackend{failJobs: map[string]error{"root": errors.New("root failed")}}
	p, id := newJobOperationPlatform(t, stack, backend, "root", true)

	p.executeJobRun(context.Background(), id, func() {})

	op, err := p.JobRunGet(context.Background(), id)
	if err != nil {
		t.Fatalf("JobRunGet: %v", err)
	}
	if op.Status != api.JobRunFailed || op.Error != "root failed" {
		t.Fatalf("operation status/error = %q/%q, want failed/root failed", op.Status, op.Error)
	}
	if got, want := backend.recordedCalls(), []string{"job:root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for _, node := range op.Nodes {
		want := api.JobRunBlocked
		if node.Name == "root" {
			want = api.JobRunFailed
		}
		if node.Status != want {
			t.Errorf("node %q status = %q, want %q", node.Name, node.Status, want)
		}
	}
}

func TestJobRunStartReturnsDurableQueryableReceipt(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"root": {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
		},
	}
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	backend := &recordingJobBackend{}
	p, err := NewWithBackends(root, backend, backend)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	receipt, err := p.JobRunStart(context.Background(), "root", nil, false)
	if err != nil {
		t.Fatalf("JobRunStart: %v", err)
	}
	if receipt.ID == "" || receipt.RootJob != "root" || len(receipt.Nodes) != 1 {
		t.Fatalf("receipt = %#v, want identified root operation", receipt)
	}

	deadline := time.Now().Add(2 * time.Second)
	for receipt.Status == api.JobRunPending || receipt.Status == api.JobRunRunning {
		if time.Now().After(deadline) {
			t.Fatalf("operation %q did not finish: %#v", receipt.ID, receipt)
		}
		time.Sleep(time.Millisecond)
		receipt, err = p.JobRunGet(context.Background(), receipt.ID)
		if err != nil {
			t.Fatalf("JobRunGet: %v", err)
		}
	}
	if receipt.Status != api.JobRunSucceeded || receipt.Output != "root output" {
		t.Fatalf("completed receipt = %#v, want root output success", receipt)
	}
	latest, err := p.LatestJobRun(context.Background())
	if err != nil {
		t.Fatalf("LatestJobRun: %v", err)
	}
	if latest == nil || latest.ID != receipt.ID || latest.Status != api.JobRunSucceeded {
		t.Fatalf("latest = %#v, want completed operation %q", latest, receipt.ID)
	}
}

// blockingJobBackend runs a job that never terminates on its own; it returns
// only when the execution context is cancelled, standing in for a container
// stuck on `docker compose wait`.
type blockingJobBackend struct {
	stubStatusBackend
}

func (b blockingJobBackend) RunJob(ctx context.Context, _ runtime.Target, _ runtime.JobSpec) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestJobRunStartTimesOutReleasesLeaseAndFailsReceipt(t *testing.T) {
	t.Setenv(jobRunTimeoutEnv, "50ms")
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"root": {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
		},
	}
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	backend := blockingJobBackend{}
	p, err := NewWithBackends(root, backend, backend)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}

	receipt, err := p.JobRunStart(context.Background(), "root", nil, false)
	if err != nil {
		t.Fatalf("JobRunStart: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for receipt.Status == api.JobRunPending || receipt.Status == api.JobRunRunning {
		if time.Now().After(deadline) {
			t.Fatalf("job run did not time out: %#v", receipt)
		}
		time.Sleep(2 * time.Millisecond)
		receipt, err = p.JobRunGet(context.Background(), receipt.ID)
		if err != nil {
			t.Fatalf("JobRunGet: %v", err)
		}
	}

	if receipt.Status != api.JobRunFailed {
		t.Fatalf("status = %q, want failed", receipt.Status)
	}
	if !strings.Contains(receipt.Error, "timed out") {
		t.Fatalf("error = %q, want a timeout message", receipt.Error)
	}
	for _, node := range receipt.Nodes {
		if node.Status == api.JobRunPending || node.Status == api.JobRunRunning {
			t.Fatalf("node %q left non-terminal after timeout: %q", node.Name, node.Status)
		}
	}

	// The single-slot mutation lease must be free again once the stuck run has
	// been bounded and finalized.
	_, release, err := p.beginMutation(context.Background(), "job")
	if err != nil {
		t.Fatalf("mutation lease not released after timeout: %v", err)
	}
	release()
	p.jobRunsMu.RLock()
	active := p.activeJobRun
	p.jobRunsMu.RUnlock()
	if active {
		t.Fatal("activeJobRun still set after timeout")
	}
}

func TestMutationLeaseIsReentrantAndRejectsOverlap(t *testing.T) {
	p, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, release, err := p.beginMutation(context.Background(), "job")
	if err != nil {
		t.Fatalf("beginMutation: %v", err)
	}
	defer release()
	_, nestedRelease, err := p.beginMutation(ctx, "stack")
	if err != nil {
		t.Fatalf("nested beginMutation: %v", err)
	}
	nestedRelease()
	if _, _, err := p.beginMutation(context.Background(), "service"); err == nil {
		t.Fatal("overlapping beginMutation succeeded")
	} else {
		if opErr := AsOperationError(err); opErr.Code != CodeConflict || !strings.Contains(err.Error(), "another stack mutation is active") {
			t.Fatalf("overlap error = %T %v, want a CONFLICT", err, err)
		}
	}
}

// waitLeaseFree waits for a finished operation to release the mutation lease:
// a receipt turns terminal just before its executor releases the lease.
func waitLeaseFree(t *testing.T, p *Platform) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, release, err := p.beginMutation(context.Background(), "probe")
		if err == nil {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mutation lease still held: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// A capture job runs only inside a backup, which chooses its output paths:
// every job entry point refuses it, with or without inputs, before anything
// runs or takes the lease, while BackupStart still captures it.
func TestCaptureJobRunsOnlyThroughBackup(t *testing.T) {
	p, _ := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"database": {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-dump"}, Env: map[string]string{"OUTPUT": "${inputs.backup_output_absolute}"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "database.dump"}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	backend := &backupCaptureBackend{root: p.root, emit: true}
	p.procBackend = backend
	ctx := context.Background()

	refused := func(name string, err error) {
		t.Helper()
		var capture *CaptureJobError
		if !errors.As(err, &capture) || capture.Job != "database" {
			t.Fatalf("%s error = %T %v, want a CaptureJobError for database", name, err, err)
		}
		if opErr := AsOperationError(err); opErr.Code != CodeInvalidInput || opErr.Job != "database" || opErr.Hint == "" {
			t.Fatalf("%s error = %+v, want INVALID_INPUT naming the job with a hint", name, opErr)
		}
	}
	for _, inputs := range []map[string]string{nil, {"backup_output": "../outside", "backup_output_absolute": "/outside"}} {
		_, err := p.JobRunStart(ctx, "database", inputs, false)
		refused("JobRunStart", err)
		_, err = p.JobRunStart(ctx, "database", inputs, true)
		refused("JobRunStart(chained)", err)
	}
	_, err = p.JobRun(ctx, "database", map[string]string{"backup_output": "../outside"})
	refused("JobRun", err)
	_, err = p.JobRunPreview(ctx, "database", false)
	refused("JobRunPreview", err)
	// The refusal comes before the lease: a busy stack still refuses it.
	_, hold, err := p.beginMutation(ctx, "stack")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.JobRunStart(ctx, "database", nil, false)
	hold()
	refused("JobRunStart(while busy)", err)

	if backend.configuration != nil {
		t.Fatal("a refused capture job reached the runtime")
	}
	if latest, err := p.LatestJobRun(ctx); err != nil || latest != nil {
		t.Fatalf("LatestJobRun = %#v, %v; want no run registered", latest, err)
	}
	_, release, err := p.beginMutation(ctx, "job")
	if err != nil {
		t.Fatalf("mutation lease held after refusals: %v", err)
	}
	release()

	req := api.BackupRequest{Version: 1, RequestID: "test-capture", Selection: api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}}
	if _, err := p.BackupStart(ctx, req); err != nil {
		t.Fatalf("BackupStart: %v", err)
	}
	if op := waitBackup(t, p, req.RequestID); op.Status != api.JobRunSucceeded || op.Artifact == nil {
		t.Fatalf("backup receipt = %+v, want a captured artifact", op)
	}
	if !strings.Contains(string(backend.configuration), "trusted-dump") {
		t.Fatalf("capture configuration = %q, want the declared capture job", backend.configuration)
	}
}

// A chained restart runs the root's dependents, but never a capture job that
// depends on a restarted node, in its preview or its run.
func TestChainedRestartLeavesCaptureJobsOut(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"deps":    {Runtime: manifest.RuntimeLocal, Command: []string{"deps"}},
			"codegen": {Runtime: manifest.RuntimeLocal, Command: []string{"codegen"}, DependsOn: []string{"deps"}},
			"dump":    {Runtime: manifest.RuntimeLocal, Command: []string{"dump"}, DependsOn: []string{"deps"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"}},
		},
	}
	root := t.TempDir()
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	backend := &recordingJobBackend{}
	p, err := NewWithBackends(root, backend, backend)
	if err != nil {
		t.Fatalf("NewWithBackends: %v", err)
	}
	preview, err := p.JobRunPreview(context.Background(), "deps", true)
	if err != nil {
		t.Fatalf("JobRunPreview: %v", err)
	}
	if want := []string{"codegen", "deps"}; !reflect.DeepEqual(preview.Jobs, want) {
		t.Fatalf("preview jobs = %v, want %v", preview.Jobs, want)
	}
	receipt, err := p.JobRunStart(context.Background(), "deps", nil, true)
	if err != nil {
		t.Fatalf("JobRunStart: %v", err)
	}
	if receipt = waitBackup(t, p, receipt.ID); receipt.Status != api.JobRunSucceeded {
		t.Fatalf("receipt = %#v, want success", receipt)
	}
	for _, call := range backend.recordedCalls() {
		if call == "job:dump" {
			t.Fatalf("calls = %v, want the capture job left out", backend.recordedCalls())
		}
	}
}

// A backup receipt is readable by its ID but never becomes the latest job run,
// and newer backups never evict that run from the retained receipts.
func TestBackupReceiptNeverBecomesLatestJobRun(t *testing.T) {
	p, req := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"deps": {Runtime: manifest.RuntimeLocal, Command: []string{"deps"}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	p.procBackend = &recordingJobBackend{}
	ctx := context.Background()
	run, err := p.JobRunStart(ctx, "deps", nil, false)
	if err != nil {
		t.Fatalf("JobRunStart: %v", err)
	}
	run = waitBackup(t, p, run.ID)
	waitLeaseFree(t, p)

	if _, err := p.BackupStart(ctx, req); err != nil {
		t.Fatalf("BackupStart: %v", err)
	}
	backup := waitBackup(t, p, req.RequestID)
	if backup.Status != api.JobRunSucceeded || len(backup.Nodes) != 2 || backup.Nodes[0].Kind != "backup" || backup.Nodes[1].Kind != "backup" {
		t.Fatalf("backup receipt = %+v, want succeeded backup nodes", backup)
	}
	latest, err := p.LatestJobRun(ctx)
	if err != nil || latest == nil || latest.ID != run.ID {
		t.Fatalf("LatestJobRun = %#v, %v; want job run %q", latest, err, run.ID)
	}

	p.jobRunsMu.Lock()
	for i := range 2 * maxRetainedJobRuns {
		p.rememberBackupRun(api.JobRunOperation{ID: fmt.Sprintf("backup-%d", i), RootJob: "backup", Status: api.JobRunSucceeded})
	}
	retained := len(p.jobRunOrder)
	p.jobRunsMu.Unlock()
	if retained != maxRetainedJobRuns {
		t.Fatalf("retained %d receipts, want %d", retained, maxRetainedJobRuns)
	}
	if latest, err := p.LatestJobRun(ctx); err != nil || latest == nil || latest.ID != run.ID {
		t.Fatalf("LatestJobRun after newer backups = %#v, %v; want job run %q", latest, err, run.ID)
	}
	if newest, err := p.JobRunGet(ctx, fmt.Sprintf("backup-%d", 2*maxRetainedJobRuns-1)); err != nil || newest.RootJob != "backup" {
		t.Fatalf("newest backup receipt = %#v, %v; want it retained", newest, err)
	}
	// An evicted backup receipt is still read from its persisted copy.
	if persisted, err := p.JobRunGet(ctx, req.RequestID); err != nil || persisted.Status != api.JobRunSucceeded {
		t.Fatalf("evicted backup receipt = %#v, %v; want its persisted copy", persisted, err)
	}
}

// Only compileCapture compiles a capture job: compile leaves it out of the
// runtime documents even when it is handed that job's inputs.
func TestCompileIncludesCaptureJobOnlyForItsBackup(t *testing.T) {
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "jobs",
		Jobs: map[string]manifest.Job{
			"deps":   {Runtime: manifest.RuntimeLocal, Command: []string{"deps"}},
			"dump":   {Runtime: manifest.RuntimeLocal, Command: []string{"dump", "${inputs.backup_output}"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"}},
			"export": {Runtime: manifest.RuntimeContainer, Image: "postgres:17", Command: []string{"export"}, Capture: &manifest.BackupCapture{Group: "service_state", Name: "app", Output: "app.tar"}},
		},
	}
	stack.Defaults()
	if err := stack.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	root := t.TempDir()
	inputs := map[string]map[string]string{"dump": {"backup_output": "state/backups/x"}, "export": {"backup_output": "state/backups/x"}}
	for _, resolveProcessSecrets := range []bool{false, true} {
		compiled, err := compile(stack, root, nil, inputs, resolveProcessSecrets)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if _, ok := compiled.ProcessCompose.Processes["dump"]; ok {
			t.Fatal("compile emitted local capture job dump")
		}
		if _, ok := compiled.Compose.Services["export"]; ok {
			t.Fatal("compile emitted container capture job export")
		}
		if _, ok := compiled.ProcessCompose.Processes["deps"]; !ok {
			t.Fatal("compile left out ordinary job deps")
		}
	}
	captured, err := compileCapture(stack, root, nil, "dump", map[string]string{"backup_output": "state/backups/x"})
	if err != nil {
		t.Fatalf("compileCapture: %v", err)
	}
	if got := captured.ProcessCompose.Processes["dump"].Command; !strings.Contains(got, "state/backups/x") {
		t.Fatalf("captured dump command = %q, want the backup's output path", got)
	}
	if _, ok := captured.Compose.Services["export"]; ok {
		t.Fatal("compileCapture emitted a capture job other than the one it runs")
	}
}

// The independent tail forces a barrier: the alphabetically earlier service
// must not resume after its own prerequisite while another selected job waits.
func holdTestStack() *manifest.Stack {
	return &manifest.Stack{
		Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "holds",
		Jobs: map[string]manifest.Job{
			"root":   {Runtime: manifest.RuntimeLocal, Command: []string{"root"}, Holds: []string{"a-web"}},
			"middle": {Runtime: manifest.RuntimeLocal, Command: []string{"middle"}, DependsOn: []string{"root"}, Holds: []string{"a-web", "b-worker"}},
			"z-tail": {Runtime: manifest.RuntimeLocal, Command: []string{"tail"}, DependsOn: []string{"middle"}},
		},
		Services: map[string]manifest.Service{
			"a-web":       {Runtime: manifest.RuntimeLocal, Command: []string{"web"}, DependsOn: []string{"root"}},
			"b-worker":    {Runtime: manifest.RuntimeContainer, Image: "worker"},
			"c-dependent": {Runtime: manifest.RuntimeLocal, Command: []string{"dependent"}, DependsOn: []string{"a-web"}},
		},
	}
}

func holdTestBackend() *recordingJobBackend {
	return &recordingJobBackend{stubStatusBackend: stubStatusBackend{statuses: []runtime.ServiceStatus{
		{Name: "a-web", State: "running"}, {Name: "b-worker", State: "running"}, {Name: "c-dependent", State: "running"},
	}}}
}

func TestJobHoldsStopUnionBeforeCommandsAndResumeOnceAfterAllJobs(t *testing.T) {
	backend := holdTestBackend()
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	p.executeJobRun(t.Context(), id, func() {})
	want := []string{"stop:a-web", "stop:b-worker", "job:root", "job:middle", "job:z-tail", "service:a-web", "service:b-worker", "service:c-dependent"}
	if got := backend.recordedCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("operation = %#v", op)
	}
	for _, node := range op.Nodes {
		if node.Name == "a-web" && node.Message != "resumed after job chain" {
			t.Fatalf("held outcome = %#v", node)
		}
	}
}

func TestJobHoldsPlainRunLeavesOnlyRequestedJobServicesStopped(t *testing.T) {
	backend := holdTestBackend()
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", false)
	p.executeJobRun(t.Context(), id, func() {})
	if got, want := backend.recordedCalls(), []string{"stop:a-web", "job:root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunSucceeded || len(op.Nodes) != 2 || !strings.Contains(op.Nodes[0].Message, "held stopped") {
		t.Fatalf("operation = %#v", op)
	}
}

func TestJobHoldsFailuresKeepSuccessfullyStoppedServicesStopped(t *testing.T) {
	for _, failJob := range []string{"root", "middle", "z-tail"} {
		t.Run(failJob, func(t *testing.T) {
			backend := holdTestBackend()
			backend.failJobs = map[string]error{failJob: errors.New("job failed")}
			p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
			p.executeJobRun(t.Context(), id, func() {})
			op, _ := p.JobRunGet(t.Context(), id)
			if op.Status != api.JobRunFailed {
				t.Fatalf("operation = %#v", op)
			}
			for _, call := range backend.recordedCalls() {
				if strings.HasPrefix(call, "service:") {
					t.Fatalf("recovered on failure: %v", backend.recordedCalls())
				}
			}
			for _, node := range op.Nodes {
				if node.Name == "a-web" || node.Name == "b-worker" {
					if node.Status != api.JobRunBlocked || !strings.Contains(node.Message, "held stopped") {
						t.Fatalf("held outcome = %#v", node)
					}
				}
			}
		})
	}
}

func TestJobHoldsPartialStopFailureRecordsEachOutcomeAndRunsNoCommands(t *testing.T) {
	backend := holdTestBackend()
	backend.failStops = map[string]error{"a-web": errors.New("stop denied")}
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	p.executeJobRun(t.Context(), id, func() {})
	if got, want := backend.recordedCalls(), []string{"stop:a-web", "stop:b-worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed || !strings.Contains(op.Error, "stop denied") {
		t.Fatalf("operation = %#v", op)
	}
	for _, node := range op.Nodes {
		switch node.Name {
		case "a-web":
			if node.Status != api.JobRunFailed || !strings.Contains(node.Message, "stop denied") {
				t.Fatalf("failed stop = %#v", node)
			}
		case "b-worker":
			if node.Status != api.JobRunBlocked || !strings.Contains(node.Message, "held stopped") {
				t.Fatalf("successful stop = %#v", node)
			}
		}
	}
}

func TestJobHoldsCancellationPreventsRecovery(t *testing.T) {
	backend := holdTestBackend()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.beforeJob = func(_ context.Context, job runtime.JobSpec) {
		if job.Name == "middle" {
			cancel()
		}
	}
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	p.executeJobRun(ctx, id, func() {})
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed {
		t.Fatalf("operation = %#v", op)
	}
	for _, call := range backend.recordedCalls() {
		if strings.HasPrefix(call, "service:") {
			t.Fatalf("resumed after cancel: %v", backend.recordedCalls())
		}
	}
	for _, node := range op.Nodes {
		if node.Status == api.JobRunPending || node.Status == api.JobRunRunning {
			t.Fatalf("unterminated node = %#v", node)
		}
	}
}

func TestJobHoldsRecoveryFailureRestoresPreviouslyResumedHolds(t *testing.T) {
	backend := holdTestBackend()
	backend.failApply = map[string]error{"b-worker": errors.New("apply denied")}
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	p.executeJobRun(t.Context(), id, func() {})
	want := []string{"stop:a-web", "stop:b-worker", "job:root", "job:middle", "job:z-tail", "service:a-web", "service:b-worker", "stop:a-web", "stop:b-worker"}
	if got := backend.recordedCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed || !strings.Contains(op.Error, "apply denied") {
		t.Fatalf("operation = %#v", op)
	}
}

func TestJobHoldsUseRuntimeOwnerForEachService(t *testing.T) {
	local, container := holdTestBackend(), holdTestBackend()
	local.beforeJob = func(_ context.Context, _ runtime.JobSpec) {
		if got := container.recordedCalls(); len(got) == 0 || got[0] != "stop:b-worker" {
			t.Fatalf("container not stopped before local job: %v", got)
		}
	}
	p, id := newJobOperationPlatform(t, holdTestStack(), local, "root", true)
	p.composeBackend = container
	p.executeJobRun(t.Context(), id, func() {})
	if got, want := container.recordedCalls(), []string{"stop:b-worker", "service:b-worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("container calls = %v, want %v", got, want)
	}
}

func TestJobHoldsRejectPrerequisiteRecoveryConflictBeforeStopping(t *testing.T) {
	for _, indirect := range []bool{false, true} {
		stack := holdTestStack()
		root := stack.Jobs["root"]
		root.DependsOn = []string{"a-web"}
		service := stack.Services["a-web"]
		service.DependsOn = nil
		stack.Services["a-web"] = service
		if indirect {
			root.DependsOn = []string{"prerequisite"}
			stack.Jobs["prerequisite"] = manifest.Job{DependsOn: []string{"a-web"}}
		}
		stack.Jobs["root"] = root
		if _, err := jobRunNodes(stack, "root", true); err == nil || !strings.Contains(err.Error(), "requires service") {
			t.Fatalf("indirect=%v: error = %v", indirect, err)
		}
	}
}

func TestJobHoldsProjectionRetainsOnlySelectedJobsHoldTargets(t *testing.T) {
	stack, err := jobOperationStack(holdTestStack(), "root", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.Jobs) != 1 || len(stack.Services) != 1 {
		t.Fatalf("projected jobs/services = %v/%v", stack.Jobs, stack.Services)
	}
	if _, ok := stack.Services["a-web"]; !ok {
		t.Fatal("held target absent from runtime projection")
	}
}

func TestJobHoldsCancellationDuringRecoveryRestoresHoldWithLiveContext(t *testing.T) {
	backend := holdTestBackend()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.beforeApply = cancel
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	p.executeJobRun(ctx, id, func() {})
	want := []string{"stop:a-web", "stop:b-worker", "job:root", "job:middle", "job:z-tail", "service:a-web", "stop:a-web"}
	if got := backend.recordedCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed {
		t.Fatalf("operation = %#v", op)
	}
	for _, node := range op.Nodes {
		if node.Name == "a-web" && (node.Status != api.JobRunBlocked || !strings.Contains(node.Message, "held stopped")) {
			t.Fatalf("restored hold = %#v", node)
		}
	}
}

func TestCanceledOrdinaryJobGraphTerminatesPendingNodes(t *testing.T) {
	backend := &recordingJobBackend{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend.beforeJob = func(_ context.Context, _ runtime.JobSpec) { cancel() }
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent, Kind: manifest.KindStack, Name: "cancel",
		Jobs: map[string]manifest.Job{
			"root": {Runtime: manifest.RuntimeLocal, Command: []string{"root"}},
			"tail": {Runtime: manifest.RuntimeLocal, Command: []string{"tail"}, DependsOn: []string{"root"}},
		},
		Services: map[string]manifest.Service{"web": {Runtime: manifest.RuntimeLocal, Command: []string{"web"}, DependsOn: []string{"tail"}}},
	}
	p, id := newJobOperationPlatform(t, stack, backend, "root", true)
	p.executeJobRun(ctx, id, func() {})
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed || !strings.Contains(op.Error, "canceled") {
		t.Fatalf("operation = %#v", op)
	}
	for _, node := range op.Nodes {
		if node.Status == api.JobRunPending || node.Status == api.JobRunRunning {
			t.Fatalf("unterminated node = %#v", node)
		}
	}
	if got, want := backend.recordedCalls(), []string{"job:root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestHeldLocalJobsUseIsolatedExecutionAndDurableReaders(t *testing.T) {
	backend := holdTestBackend()
	backend.statuses = append(backend.statuses, runtime.ServiceStatus{Name: "root", State: "completed", ExitCode: exitCode(9), Generation: "old-native-run"})
	isolated := 0
	backend.beforeJob = func(_ context.Context, spec runtime.JobSpec) {
		if !spec.Transient {
			t.Fatalf("held job %s used the shared supervisor", spec.Name)
		}
		isolated++
	}
	stack := holdTestStack()
	p, id := newJobOperationPlatform(t, stack, backend, "root", true)
	p.executeJobRun(t.Context(), id, func() {})
	if isolated != 3 {
		t.Fatalf("isolated jobs = %d, want 3", isolated)
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("operation = %#v", op)
	}
	for _, node := range op.Nodes {
		if node.Kind == "job" && (!node.Isolated || node.Output != node.Name+" output" || node.ExitCode == nil || *node.ExitCode != 0) {
			t.Fatalf("attempt = %#v", node)
		}
	}
	// A fresh Platform reads the same durable owner without an auxiliary index.
	reloaded, err := NewWithBackends(p.root, backend, backend)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := reloaded.LatestJobRun(t.Context())
	if err != nil || latest == nil || latest.ID != id {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	status, err := reloaded.StackStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Jobs["root"]; got.Status != api.JobCompleted || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("durable job status = %#v", got)
	}
	compiled, err := Compile(stack, p.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := reloaded.dependencySatisfied(t.Context(), stack, compiled, "root"); err != nil || !ready {
		t.Fatalf("durable prerequisite = %v, %v", ready, err)
	}
	logs, err := reloaded.StackLogs(t.Context(), []string{"root"}, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for chunk := range logs {
		output.WriteString(chunk)
	}
	if output.String() != "root output" {
		t.Fatalf("durable logs = %q", output.String())
	}
	// A later ordinary native generation takes precedence everywhere.
	backend.statuses[len(backend.statuses)-1] = runtime.ServiceStatus{Name: "root", State: "completed", ExitCode: exitCode(7), Generation: "new-native-run"}
	status, err = reloaded.StackStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Jobs["root"]; got.Status != api.JobFailed || got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("new native status = %#v", got)
	}
	if ready, err := reloaded.dependencySatisfied(t.Context(), stack, compiled, "root"); err != nil || ready {
		t.Fatalf("new native prerequisite = %v, %v", ready, err)
	}
	logs, err = reloaded.StackLogs(t.Context(), []string{"root"}, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range logs {
		t.Fatalf("stale isolated output used over native reader: %q", chunk)
	}
}

func TestIsolatedFailedAndInterruptedAttemptsDoNotSatisfyPrerequisites(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			backend := holdTestBackend()
			backend.statuses = append(backend.statuses, runtime.ServiceStatus{Name: "root", State: "completed", ExitCode: exitCode(0), Generation: "old"})
			stack := holdTestStack()
			p, _ := newJobOperationPlatform(t, stack, backend, "root", false)
			op := api.JobRunOperation{ID: "attempt", StartedAt: time.Now(), RootJob: "root", Status: api.JobRunFailed, Error: "attempt failed", Nodes: []api.JobRunNode{{Name: "root", Kind: "job", Isolated: true, RuntimeGeneration: "old", Status: api.JobRunFailed, Output: "safe failure output"}}}
			if interrupted {
				op.Status = api.JobRunRunning
				op.Nodes[0].Status = api.JobRunRunning
			}
			if err := p.persistJobReceipt(op); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewWithBackends(p.root, backend, backend)
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := Compile(stack, p.root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if ready, err := reloaded.dependencySatisfied(t.Context(), stack, compiled, "root"); err != nil || ready {
				t.Fatalf("failed prerequisite = %v, %v", ready, err)
			}
			status, err := reloaded.StackStatus(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if status.Jobs["root"].Status != api.JobFailed {
				t.Fatalf("job = %#v", status.Jobs["root"])
			}
		})
	}
}

func TestOrdinaryJobRunRetainsNativeExecutionAfterHeldAttempt(t *testing.T) {
	backend := holdTestBackend()
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", false)
	p.executeJobRun(t.Context(), id, func() {})
	stack := holdTestStack()
	stack.Jobs["ordinary"] = manifest.Job{Runtime: manifest.RuntimeLocal, Command: []string{"ordinary"}, DependsOn: []string{"root"}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	nodes, err := jobRunNodes(stack, "ordinary", false)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(stack, p.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend.beforeJob = func(_ context.Context, job runtime.JobSpec) {
		if job.Transient {
			t.Fatal("ordinary run was changed to isolated")
		}
	}
	p.jobRunsMu.Lock()
	p.rememberJobRun(api.JobRunOperation{ID: "ordinary", RootJob: "ordinary", Nodes: nodes, Status: api.JobRunPending, StartedAt: time.Now()}, stack, compiled)
	p.jobRunsMu.Unlock()
	p.executeJobRun(t.Context(), "ordinary", func() {})
	op, _ := p.JobRunGet(t.Context(), "ordinary")
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("operation = %#v", op)
	}
}

func TestIsolatedLogsFollowSelectedAttemptAndHonorTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, id := newJobOperationPlatform(t, holdTestStack(), holdTestBackend(), "root", false)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		logs, err := p.StackLogs(ctx, []string{"root"}, true, 2)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		p.updateJobRun(id, func(op *api.JobRunOperation) {
			for i := range op.Nodes {
				if op.Nodes[i].Name == "root" {
					op.Nodes[i].Output = "first\nsecond\nthird\n"
					op.Nodes[i].Status = api.JobRunSucceeded
				}
			}
		})
		p.jobRunsMu.Lock()
		p.jobRuns["later"] = api.JobRunOperation{ID: "later", StartedAt: time.Now().Add(time.Second), Nodes: []api.JobRunNode{{Name: "root", Kind: "job", Isolated: true, Status: api.JobRunSucceeded, Output: "unrelated\n"}}}
		p.jobRunsMu.Unlock()
		var output strings.Builder
		for chunk := range logs {
			output.WriteString(chunk)
		}
		if output.String() != "second\nthird\n" {
			t.Fatalf("follow = %q", output.String())
		}
	})
}

func TestIsolatedReceiptFailurePreventsCommands(t *testing.T) {
	backend := holdTestBackend()
	p, id := newJobOperationPlatform(t, holdTestStack(), backend, "root", true)
	if err := os.WriteFile(filepath.Join(p.root, "state"), []byte("cannot contain receipts"), 0600); err != nil {
		t.Fatal(err)
	}
	p.executeJobRun(t.Context(), id, func() {})
	for _, call := range backend.recordedCalls() {
		if strings.HasPrefix(call, "job:") || strings.HasPrefix(call, "service:") {
			t.Fatalf("command after persistence failure: %v", backend.recordedCalls())
		}
	}
	op, _ := p.JobRunGet(t.Context(), id)
	if op.Status != api.JobRunFailed || !strings.Contains(op.Error, "persist job operation") {
		t.Fatalf("operation = %#v", op)
	}
}
