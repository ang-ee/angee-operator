package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	"github.com/ang-ee/angee-operator/internal/logctx"
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
