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
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

type recordingJobBackend struct {
	stubStatusBackend
	mu       sync.Mutex
	calls    []string
	failJobs map[string]error
}

func (b *recordingJobBackend) RunJob(_ context.Context, _ runtime.Target, job runtime.JobSpec) ([]byte, error) {
	b.mu.Lock()
	b.calls = append(b.calls, "job:"+job.Name)
	err := b.failJobs[job.Name]
	b.mu.Unlock()
	return []byte(job.Name + " output"), err
}

func (b *recordingJobBackend) Apply(_ context.Context, target runtime.Target) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "service:"+target.Services[0])
	return nil
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
