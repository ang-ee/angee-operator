package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

	p.executeJobRun(context.Background(), id, nil, func() {})

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

	p.executeJobRun(context.Background(), id, nil, func() {})

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
		var invalid *InvalidInputError
		if !errors.As(err, &invalid) || invalid.Field != "service" {
			t.Fatalf("overlap error = %T %v, want service InvalidInputError", err, err)
		}
	}
}
