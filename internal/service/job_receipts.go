package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// persistJobReceipt retains operation evidence across daemon restarts. It never
// retains runtime snapshots or commands from untrusted transport requests.
func (p *Platform) persistJobReceipt(op api.JobRunOperation) error {
	if !operationIDPattern.MatchString(op.ID) {
		return &InvalidInputError{Field: "request_id", Reason: "must be 1..128 letters, digits, underscores or hyphens"}
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err = root.MkdirAll("state/job-runs", 0700); err != nil {
		return err
	}
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	name := "state/job-runs/" + op.ID + ".json"
	if err := root.Remove(name + ".tmp"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(name+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name + ".tmp") }()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(name+".tmp", name)
}

func (p *Platform) readJobReceipt(ctx context.Context, id string, rootLocked bool) (api.JobRunOperation, error) {
	if !operationIDPattern.MatchString(id) {
		return api.JobRunOperation{}, &NotFoundError{Kind: "job run", Name: id}
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return api.JobRunOperation{}, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("state/job-runs/" + id + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return api.JobRunOperation{}, &NotFoundError{Kind: "job run", Name: id}
	}
	if err != nil {
		return api.JobRunOperation{}, err
	}
	var op api.JobRunOperation
	if err = json.Unmarshal(data, &op); err != nil {
		return op, err
	}
	if op.ID != id {
		return op, errors.New("job receipt identity mismatch")
	}
	if op.Status == api.JobRunPending || op.Status == api.JobRunRunning {
		// Another Platform or daemon may still own this operation. Only recover an
		// interrupted receipt while holding the same root lock execution owns.
		if !rootLocked {
			lockCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
			defer cancel()
			lock := fslock.RootLock(p.root)
			if err := lock.Lock(lockCtx); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return op, nil
				}
				return op, err
			}
			defer func() { _ = lock.Unlock() }()
		}
		latest, err := root.ReadFile("state/job-runs/" + id + ".json")
		if err != nil {
			return op, err
		}
		if err = json.Unmarshal(latest, &op); err != nil {
			return op, err
		}
		if op.Status != api.JobRunPending && op.Status != api.JobRunRunning {
			return op, nil
		}
		now := time.Now().UTC()
		op.Status = api.JobRunFailed
		op.EndedAt = &now
		op.Error = "operator stopped before operation completion; no completed artifact was published"
		op.ErrorCode = CodeJobFailed
		// An interrupted backup never ran its own cleanup. A removal that fails,
		// as on files a capture left that the operator may not unlink, would fail
		// again on every read, so the receipt fails anyway and names what is left.
		if IsBackupReceipt(op) {
			if cleanupErr := removeInterruptedBackup(root, id); cleanupErr != nil {
				logctx.From(ctx).WarnContext(ctx, "interrupted backup cleanup failed", "id", id, "error", logctx.RedactText(cleanupErr.Error()))
				op.Error += "; its files could not be removed: remove " + backupStagingDirectory + "/" + id + " and " + backupArchiveDirectory + "/" + id + ".zip"
			}
		}
		for i := range op.Nodes {
			if op.Nodes[i].Status == api.JobRunPending || op.Nodes[i].Status == api.JobRunRunning {
				op.Nodes[i].Status = api.JobRunFailed
			}
		}
		if err = p.persistJobReceipt(op); err != nil {
			return op, err
		}
	}
	return op, nil
}

// jobOperations reads the existing receipt owner plus its in-memory front. It
// does not retain another index; atomic receipts remain authoritative after a
// daemon restart, and JobRunGet owns interrupted-operation recovery.
func (p *Platform) jobOperations(ctx context.Context) ([]api.JobRunOperation, error) {
	ids := map[string]bool{}
	p.jobRunsMu.RLock()
	for id := range p.jobRuns {
		ids[id] = true
	}
	p.jobRunsMu.RUnlock()
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open("state/job-runs")
	if err == nil {
		entries, readErr := directory.ReadDir(-1)
		_ = directory.Close()
		if readErr != nil {
			return nil, readErr
		}
		for _, entry := range entries {
			if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && !entry.IsDir() && operationIDPattern.MatchString(id) {
				ids[id] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	operations := make([]api.JobRunOperation, 0, len(ids))
	for _, id := range sortedKeys(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		op, err := p.JobRunGet(ctx, id)
		if err != nil {
			return nil, err
		}
		if !IsBackupReceipt(op) {
			operations = append(operations, op)
		}
	}
	return operations, nil
}

type jobAttempt struct {
	operation string
	started   time.Time
	node      api.JobRunNode
}

func (attempt jobAttempt) owns(observed runtime.ServiceStatus) bool {
	return attempt.node.Isolated && observed.State != "unknown" && (observed.Generation == "" || observed.Generation == attempt.node.RuntimeGeneration)
}

func (attempt jobAttempt) runtimeState(name, backend string) runtime.ServiceStatus {
	state := runtime.ServiceStatus{Name: name, Runtime: backend, ExitCode: attempt.node.ExitCode, Generation: attempt.node.RuntimeGeneration}
	switch attempt.node.Status {
	case api.JobRunPending:
		state.State = "pending"
	case api.JobRunRunning:
		state.State = "running"
	case api.JobRunSucceeded:
		state.State = "completed"
	case api.JobRunBlocked:
		state.State = "skipped"
	case api.JobRunFailed:
		state.State = "error"
		if state.ExitCode != nil {
			state.State = "completed"
		}
	}
	return state
}

func (p *Platform) latestJobAttempts(ctx context.Context) (map[string]jobAttempt, error) {
	operations, err := p.jobOperations(ctx)
	if err != nil {
		return nil, err
	}
	attempts := map[string]jobAttempt{}
	for _, op := range operations {
		for _, node := range op.Nodes {
			if node.Kind != "job" {
				continue
			}
			previous, exists := attempts[node.Name]
			if exists && (op.StartedAt.Before(previous.started) || op.StartedAt.Equal(previous.started) && op.ID < previous.operation) {
				continue
			}
			if node.Status == api.JobRunFailed && node.Message == "" {
				node.Message = op.Error
			}
			attempts[node.Name] = jobAttempt{operation: op.ID, started: op.StartedAt, node: node}
		}
	}
	return attempts, nil
}

func (p *Platform) persistJobRun(id string) error {
	op, err := p.JobRunGet(context.Background(), id)
	if err != nil {
		return err
	}
	if err := p.persistJobReceipt(op); err != nil {
		return fmt.Errorf("persist job operation: %w", err)
	}
	return nil
}

func (p *Platform) prepareIsolatedJobAttempts(ctx context.Context, id string, stack *manifest.Stack, compiled *CompiledStack, nodes []api.JobRunNode) error {
	isolated := false
	for _, node := range nodes {
		if !node.Isolated {
			continue
		}
		isolated = true
		if validator, ok := p.procBackend.(runtime.JobValidator); ok {
			if err := validator.ValidateJob(runtime.JobSpec{Name: node.Name, Transient: true}); err != nil {
				return err
			}
		}
	}
	if !isolated {
		return nil
	}
	backend, request, err := p.operationRuntime(stack, compiled, manifest.RuntimeLocal)
	if err != nil {
		return err
	}
	statuses, err := backend.Status(ctx, request)
	if err != nil && !processComposeNotRunning(err) {
		return err
	}
	generations := map[string]string{}
	for _, status := range statuses {
		generations[status.Name] = status.Generation
	}
	p.updateJobRun(id, func(op *api.JobRunOperation) {
		for i := range op.Nodes {
			if op.Nodes[i].Isolated {
				op.Nodes[i].RuntimeGeneration = generations[op.Nodes[i].Name]
			}
		}
	})
	return nil
}

func (p *Platform) restoreJobHolds(ctx context.Context, id string, stack *manifest.Stack, compiled *CompiledStack, targets map[string]bool) error {
	if len(targets) == 0 {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtime.GracefulWaitDelay)
	defer cancel()
	op, err := p.JobRunGet(cleanup, id)
	if err != nil {
		return err
	}
	var failures []error
	for i, node := range op.Nodes {
		if !targets[node.Name] {
			continue
		}
		if err := p.stopJobHeldService(cleanup, stack, compiled, node.Name); err != nil {
			p.setNode(id, i, api.JobRunFailed, "recovery failed; hold stop failed: "+err.Error())
			failures = append(failures, fmt.Errorf("restore hold on service %q: %w", node.Name, err))
		} else {
			p.setNode(id, i, api.JobRunBlocked, "held stopped; service recovery failed")
		}
	}
	return errors.Join(failures...)
}

// isolatedJobLogs follows the selected attempt, never an unrelated later run.
// Native foreground output is buffered and redacted before receipt publication.
func (p *Platform) isolatedJobLogs(ctx context.Context, name string, attempt jobAttempt, tail int, follow bool, width int) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		node := attempt.node
		if follow {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for node.Status == api.JobRunPending || node.Status == api.JobRunRunning {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					op, err := p.JobRunGet(ctx, attempt.operation)
					if err != nil {
						logctx.From(ctx).Warn("read isolated job logs", "error", err)
						return
					}
					for _, current := range op.Nodes {
						if current.Kind == "job" && current.Name == name {
							node = current
							break
						}
					}
				}
			}
		}
		text := node.Output
		lines := strings.SplitAfter(text, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if tail > 0 && len(lines) > tail {
			text = strings.Join(lines[len(lines)-tail:], "")
		}
		if width > 0 {
			text = runtime.PrefixLines(text, name, width)
		}
		if text != "" {
			select {
			case out <- text:
			case <-ctx.Done():
			}
		}
	}()
	return out
}
