package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/fslock"
	gitx "github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

func backupFixture(t *testing.T) (*Platform, api.BackupRequest) {
	t.Helper()
	root := t.TempDir()
	stack := &manifest.Stack{Version: 1, Kind: "stack", Name: "small", Persist: map[string]manifest.PersistPath{"app-data": {Subpath: "./data", Scope: "stack"}, "pgdata": {Subpath: "./data/pgdata", Scope: "stack", Backup: "exclude"}}}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"data/files/example.txt": "small payload", "data/pgdata/physical": "never copy a live PostgreSQL store", ".env": "YAMLCONF_SECRET_KEY=original-recovery-key\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	return p, api.BackupRequest{Version: 1, RequestID: "test-backup", Selection: api.BackupSelection{Version: 1, Storage: api.BackupGroupSelection{Mode: "all"}}}
}
func waitBackup(t *testing.T, p *Platform, id string) api.JobRunOperation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		op, err := p.JobRunGet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != api.JobRunPending && op.Status != api.JobRunRunning {
			return op
		}
		select {
		case <-ctx.Done():
			t.Fatal("backup did not complete")
		case <-ticker.C:
		}
	}
}
func TestBackupExcludesPhysicalStateAndRetainsVerifiedRecovery(t *testing.T) {
	p, req := backupFixture(t)
	preview, err := p.BackupPreview(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.HasUnknownSizes || preview.EstimatedBytes != int64(len("small payload")) {
		t.Fatalf("preview=%+v", preview)
	}
	if _, err = p.BackupStart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("receipt=%+v", op)
	}
	file, artifact, err := p.BackupArchive(context.Background(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if artifact.Path != "backups/archives/test-backup.zip" || artifact.ManifestVersion != 1 || artifact.SHA256 == "" {
		t.Fatalf("artifact=%+v", artifact)
	}
	archive, err := zip.OpenReader(filepath.Join(p.root, artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	seen := map[string]bool{}
	for _, entry := range archive.File {
		seen[entry.Name] = true
		if strings.Contains(entry.Name, "physical") {
			t.Fatalf("unsafe entry %q", entry.Name)
		}
	}
	for _, name := range []string{"storage/app-data/files/example.txt", "manifest.json", "recovery/secrets.env", "recovery/angee.yaml"} {
		if !seen[name] {
			t.Errorf("missing %s", name)
		}
	}
	fresh, err := New(p.root)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := fresh.JobRunGet(context.Background(), op.ID)
	if err != nil || retained.Status != api.JobRunSucceeded || retained.Artifact.SHA256 != artifact.SHA256 {
		t.Fatalf("restart receipt=%+v err=%v", retained, err)
	}
	again, err := fresh.BackupStart(context.Background(), req)
	if err != nil || again.ID != op.ID {
		t.Fatalf("idempotency receipt=%+v err=%v", again, err)
	}
	req.Selection.Storage = api.BackupGroupSelection{Mode: "selected", Names: []string{"app-data"}}
	_, err = fresh.BackupStart(context.Background(), req)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("changed request error=%v", err)
	}
	// Public receipt values are snapshots, never shared mutable catalog state.
	retained.Artifact.Manifest.Entries[0].Path = "mutated"
	check, _ := p.JobRunGet(context.Background(), op.ID)
	if check.Artifact.Manifest.Entries[0].Path == "mutated" {
		t.Fatal("artifact snapshot aliases mutable operation state")
	}
}
func TestBackupInterruptedReceiptDoesNotRetryUnderSameRequest(t *testing.T) {
	p, req := backupFixture(t)
	pending := api.JobRunOperation{ID: req.RequestID, RootJob: "backup", Status: api.JobRunRunning, Nodes: []api.JobRunNode{{Name: "capture", Status: api.JobRunRunning}}}
	if err := p.persistJobReceipt(pending); err != nil {
		t.Fatal(err)
	}
	fresh, _ := New(p.root)
	op, err := fresh.JobRunGet(context.Background(), req.RequestID)
	if err != nil || op.Status != api.JobRunFailed || op.Artifact != nil {
		t.Fatalf("receipt=%+v err=%v", op, err)
	}
}

// An operator stopped mid-backup never runs the backup's own cleanup.
// Recovering its receipt removes what it left: its staging, with a plaintext
// dump and a partial archive, and an archive published before the receipt that
// would have completed it. Nothing goes while another holder of the root lock
// may still be running it, and another backup's files, a completed backup's
// archive and files named after a receipt that is not a backup's stay.
func TestBackupInterruptedReceiptRemovesItsStagingAndUnreceiptedArchive(t *testing.T) {
	p, req := backupFixture(t)
	write := func(name string) {
		t.Helper()
		file := filepath.Join(p.root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("SENSITIVE DUMP ROWS"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(name string) bool {
		t.Helper()
		_, err := os.Lstat(filepath.Join(p.root, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	interrupted := api.JobRunOperation{ID: req.RequestID, RootJob: "backup", Status: api.JobRunRunning, Nodes: []api.JobRunNode{{Name: "capture", Kind: "backup", Status: api.JobRunSucceeded}, {Name: "verify", Kind: "backup", Status: api.JobRunRunning}}}
	completed := api.JobRunOperation{ID: "completed", RootJob: "backup", Status: api.JobRunSucceeded, Nodes: []api.JobRunNode{{Name: "capture", Kind: "backup", Status: api.JobRunSucceeded}, {Name: "verify", Kind: "backup", Status: api.JobRunSucceeded}}}
	jobRun := api.JobRunOperation{ID: "job-run", RootJob: "backup", Status: api.JobRunRunning, Nodes: []api.JobRunNode{{Name: "backup", Kind: "job", Status: api.JobRunRunning}}}
	for _, op := range []api.JobRunOperation{interrupted, completed, jobRun} {
		if err := p.persistJobReceipt(op); err != nil {
			t.Fatal(err)
		}
	}
	left := []string{"state/backups/" + req.RequestID + "/captures/database/database.dump", "state/backups/" + req.RequestID + "/archive.zip", "backups/archives/" + req.RequestID + ".zip"}
	kept := []string{"backups/archives/completed.zip", "state/backups/job-run/archive.zip", "backups/archives/job-run.zip", "state/backups/other/captures/database/database.dump", "backups/archives/other.zip"}
	for _, name := range append(slices.Clone(left), kept...) {
		write(name)
	}
	fresh, err := New(p.root)
	if err != nil {
		t.Fatal(err)
	}
	// While another process holds the root lock, the backup may be its own.
	lock := fslock.RootLock(p.root)
	if err := lock.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	op, err := fresh.JobRunGet(t.Context(), interrupted.ID)
	if unlockErr := lock.Unlock(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if err != nil || op.Status != api.JobRunRunning {
		t.Fatalf("receipt read under another holder's root lock = %+v, %v; want it still running", op, err)
	}
	for _, name := range left {
		if !exists(name) {
			t.Fatalf("%s was removed while another holder of the root lock may run its backup", name)
		}
	}
	for _, want := range []api.JobRunOperation{interrupted, completed, jobRun} {
		status := api.JobRunFailed
		if want.Status == api.JobRunSucceeded {
			status = api.JobRunSucceeded
		}
		if op, err := fresh.JobRunGet(t.Context(), want.ID); err != nil || op.Status != status {
			t.Fatalf("receipt %s = %+v, %v; want it %s", want.ID, op, err, status)
		}
	}
	for _, name := range append(left, "state/backups/"+req.RequestID) {
		if exists(name) {
			t.Errorf("%s survived its interrupted backup's recovery", name)
		}
	}
	for _, name := range kept {
		if !exists(name) {
			t.Errorf("%s was removed by another receipt's recovery", name)
		}
	}
}

// Files an interrupted backup left that the operator cannot remove, such as a
// directory a root container capture created, do not keep its receipt from
// failing: the receipt answers, and names what is left to remove.
func TestBackupInterruptedReceiptFailsWhenItsFilesCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes any file")
	}
	p, req := backupFixture(t)
	interrupted := api.JobRunOperation{ID: req.RequestID, RootJob: "backup", Status: api.JobRunRunning, Nodes: []api.JobRunNode{{Name: "capture", Kind: "backup", Status: api.JobRunRunning}, {Name: "verify", Kind: "backup", Status: api.JobRunPending}}}
	if err := p.persistJobReceipt(interrupted); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(p.root, "state/backups", req.RequestID, "captures/database")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "database.dump"), []byte("SENSITIVE DUMP ROWS"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	fresh, err := New(p.root)
	if err != nil {
		t.Fatal(err)
	}
	op, err := fresh.JobRunGet(t.Context(), req.RequestID)
	if err != nil || op.Status != api.JobRunFailed || !strings.Contains(op.Error, "remove state/backups/"+req.RequestID) {
		t.Fatalf("receipt = %+v, %v; want it failed, naming what is left", op, err)
	}
	if strings.Contains(op.Error, p.root) {
		t.Fatalf("receipt error %q names the stack root", op.Error)
	}
	again, err := New(p.root)
	if err != nil {
		t.Fatal(err)
	}
	if op, err := again.JobRunGet(t.Context(), req.RequestID); err != nil || op.Status != api.JobRunFailed {
		t.Fatalf("receipt read again = %+v, %v; want it failed", op, err)
	}
}

func TestBackupDoesNotFollowExternalFileOrPersistLinks(t *testing.T) {
	p, req := backupFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("external secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.root, "data", "external")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.BackupStart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("receipt=%+v", op)
	}
	archive, err := zip.OpenReader(filepath.Join(p.root, op.Artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		reader, _ := entry.Open()
		data, _ := io.ReadAll(reader)
		reader.Close()
		if strings.Contains(string(data), "external secret") {
			t.Fatal("followed external symlink")
		}
	}
	// A resource root replaced by a link is unsupported rather than dereferenced.
	if err := os.RemoveAll(filepath.Join(p.root, "data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.root, "data")); err != nil {
		t.Fatal(err)
	}
	preview, err := p.BackupPreview(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Resources[0].Status != "unsupported" {
		t.Fatalf("preview=%+v", preview)
	}
}

type backupCaptureBackend struct {
	stubStatusBackend
	root          string
	emit          bool
	configuration []byte
}

func (b *backupCaptureBackend) RunJob(_ context.Context, _ runtime.Target, spec runtime.JobSpec) ([]byte, error) {
	b.configuration = spec.Configuration
	if b.emit {
		dir := filepath.Join(b.root, "state/backups/test-capture/captures/database")
		if err := os.WriteFile(filepath.Join(dir, "database.dump"), []byte("logical one-database capture"), 0600); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
func TestBackupCaptureRequiresDeclaredNonemptyOutput(t *testing.T) {
	for _, emit := range []bool{true, false} {
		t.Run(map[bool]string{true: "complete", false: "missing"}[emit], func(t *testing.T) {
			p, _ := backupFixture(t)
			stack, _ := p.LoadStack()
			stack.Jobs = map[string]manifest.Job{"database": {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-dump", "--database", "stack_owned_only"}, Env: map[string]string{"OUTPUT": "${inputs.backup_output_absolute}"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "stack_owned_only", Output: "database.dump", ExcludePersist: []string{"pgdata"}}}}
			if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
				t.Fatal(err)
			}
			backend := &backupCaptureBackend{root: p.root, emit: emit}
			p.procBackend = backend
			req := api.BackupRequest{Version: 1, RequestID: "test-capture", Selection: api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}}
			preview, err := p.BackupPreview(context.Background(), req)
			if err != nil || !preview.HasUnknownSizes {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			if _, err = p.BackupStart(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			op := waitBackup(t, p, req.RequestID)
			if emit {
				if op.Status != api.JobRunSucceeded || op.Artifact == nil {
					t.Fatalf("receipt=%+v", op)
				}
				if !strings.Contains(string(backend.configuration), "stack_owned_only") {
					t.Fatal("capture did not compose declared runtime job")
				}
			} else if op.Status != api.JobRunFailed || op.Artifact != nil {
				t.Fatalf("receipt=%+v", op)
			}
		})
	}
}
func TestBackupPortableGitWorktreeAndSharedJJ(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj unavailable")
	}
	primary := t.TempDir()
	run := func(dir, tool string, args ...string) {
		t.Helper()
		cmd := exec.Command(tool, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "JJ_USER=backup-test", "JJ_EMAIL=backup@example.invalid", "JJ_CONFIG="+filepath.Join(t.TempDir(), "config.toml"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v %s", tool, args, err, out)
		}
	}
	run(primary, "jj", "git", "init", "--colocate")
	if err := os.WriteFile(filepath.Join(primary, "local-only.txt"), []byte("unpublished local payload"), 0600); err != nil {
		t.Fatal(err)
	}
	run(primary, "jj", "new", "-m", "local history")
	p, _ := backupFixture(t)
	slot := filepath.Join(p.root, "workspaces/dev/work")
	if err := os.MkdirAll(filepath.Dir(slot), 0700); err != nil {
		t.Fatal(err)
	}
	run(primary, "jj", "workspace", "add", "--name", "backup-secondary", slot)
	stack, _ := p.LoadStack()
	stack.Sources = map[string]manifest.Source{"work": {Kind: "local", Path: primary}}
	stack.Workspaces = map[string]manifest.Workspace{"dev": {Template: "test", Sources: map[string]manifest.WorkspaceSource{"work": {Source: "work", Subpath: "work"}}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req := api.BackupRequest{Version: 1, RequestID: "portable-jj", Selection: api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "selected", Names: []string{"workspace:dev/work"}}, Policy: "bundle"}}}
	preview, err := p.BackupPreview(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range preview.Resources {
		if resource.Name == "workspace:dev/work" && resource.Status != "included" {
			t.Fatalf("JJ preview=%+v", resource)
		}
	}
	if _, err := p.BackupStart(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("receipt=%+v", op)
	}
	if op.Artifact.Manifest.SourceRefs["workspace:dev/work"].JJRevision == "" {
		t.Fatal("missing native JJ commit/operation evidence")
	}
	data, _ := json.Marshal(op.Artifact.Manifest)
	if !strings.Contains(string(data), "jj-repo/store/") || !strings.Contains(string(data), "local-only.txt") {
		t.Fatalf("missing JJ recovery components %s", data)
	}
	restored := t.TempDir()
	archive, err := zip.OpenReader(filepath.Join(p.root, op.Artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	prefix := "sources/workspace/dev/work/"
	for _, entry := range archive.File {
		relative := ""
		for from, to := range map[string]string{"worktree/": "", "repository/git/": ".git/", "repository/jj-repo/": ".jj/repo/", "repository/jj-workspace/": ".jj/"} {
			if strings.HasPrefix(entry.Name, prefix+from) {
				relative = to + strings.TrimPrefix(entry.Name, prefix+from)
				break
			}
		}
		if relative == "" {
			continue
		}
		destination := filepath.Join(restored, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, entry.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(restored, ".jj/repo/store/git_target"), []byte("../../../.git"), 0600); err != nil {
		t.Fatal(err)
	}
	evidence := op.Artifact.Manifest.SourceRefs["workspace:dev/work"]
	run(restored, "git", "fsck", "--full", "--no-reflogs")
	run(restored, "jj", "--ignore-working-copy", "--at-op", evidence.JJOperation, "log", "--no-graph", "-r", evidence.JJRevision, "-T", "commit_id")
	if data, err := os.ReadFile(filepath.Join(restored, "local-only.txt")); err != nil || string(data) != "unpublished local payload" {
		t.Fatalf("portable worktree=%q err=%v", data, err)
	}
	req.RequestID = "jj-remote"
	req.Selection.Sources.Policy = "fetch_published"
	_, err = p.BackupStart(context.Background(), req)
	if err == nil {
		t.Fatal("remote-only accepted unpublished JJ history")
	}
}

func TestBackupRejectsExternalAncestorAndNestedExclusion(t *testing.T) {
	t.Run("ancestor", func(t *testing.T) {
		p, req := backupFixture(t)
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(outside, "files"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(p.root, "data")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(p.root, "data")); err != nil {
			t.Fatal(err)
		}
		stack, _ := p.LoadStack()
		stack.Persist = map[string]manifest.PersistPath{"files": {Subpath: "data/files", Scope: "stack"}}
		if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
			t.Fatal(err)
		}
		preview, err := p.BackupPreview(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if preview.Resources[0].Status != "unsupported" {
			t.Fatalf("preview=%+v", preview)
		}
	})
	t.Run("nested", func(t *testing.T) {
		p, req := backupFixture(t)
		stack, _ := p.LoadStack()
		stack.Persist["files"] = manifest.PersistPath{Subpath: "data/files", Scope: "stack"}
		if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
			t.Fatal(err)
		}
		req.Selection.Storage.Exclude = []string{"files"}
		if _, err := p.BackupStart(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		op := waitBackup(t, p, req.RequestID)
		if op.Status != api.JobRunSucceeded {
			t.Fatalf("%+v", op)
		}
		for _, entry := range op.Artifact.Manifest.Entries {
			if strings.HasSuffix(entry.Path, "example.txt") {
				t.Fatalf("excluded nested payload %s", entry.Path)
			}
		}
	})
}
func TestBackupNeverRegeneratesMissingRecoveryKey(t *testing.T) {
	p, req := backupFixture(t)
	stack, _ := p.LoadStack()
	stack.Secrets = map[string]manifest.Secret{"secret-key": {Generated: true}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.root, ".env")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunFailed || op.Artifact != nil {
		t.Fatalf("%+v", op)
	}
	if _, err := os.Stat(filepath.Join(p.root, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key file was replaced: %v", err)
	}
}
func TestBackupJobReceiptSnapshotWhileUpdatingNodes(t *testing.T) {
	p, _ := backupFixture(t)
	p.jobRuns["race"] = api.JobRunOperation{ID: "race", Nodes: []api.JobRunNode{{Name: "capture"}}}
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for i := 0; i < 1000; i++ {
			p.jobRunsMu.Lock()
			op := p.jobRuns["race"]
			op.Nodes[0].Message = "progress"
			p.jobRuns["race"] = op
			p.jobRunsMu.Unlock()
		}
	}()
	go func() {
		defer group.Done()
		for i := 0; i < 1000; i++ {
			if _, err := p.JobRunGet(t.Context(), "race"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	group.Wait()
}

func TestBackupDiscoveryReportsUnsupportedPersistenceWithoutGuessingStatelessServices(t *testing.T) {
	p, req := backupFixture(t)
	stack, _ := p.LoadStack()
	stack.Volumes = map[string]manifest.Volume{"queue-data": {}}
	stack.Services = map[string]manifest.Service{"queue": {Runtime: manifest.RuntimeContainer, Image: "example", Mounts: []string{"volume://queue-data:/data"}}, "files": {Runtime: manifest.RuntimeContainer, Image: "example", Mounts: []string{"bind://./data:/data"}}, "stateless": {Runtime: manifest.RuntimeContainer, Image: "example"}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection.ServiceState = api.BackupGroupSelection{Mode: "all"}
	preview, err := p.BackupPreview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, resource := range preview.Resources {
		seen[resource.Group+":"+resource.Name] = resource.Status
	}
	for _, key := range []string{"storage:volume:queue-data", "service_state:queue", "service_state:files"} {
		if seen[key] != "unsupported" {
			t.Errorf("%s=%q", key, seen[key])
		}
	}
	if _, exists := seen["service_state:stateless"]; exists {
		t.Fatal("guessed a persistent capability for stateless service")
	}
	if _, err := p.BackupStart(t.Context(), req); err == nil {
		t.Fatal("included unsupported resource was silently omitted")
	}
}

func TestBackupDiscoveryDoesNotDuplicateCapturedDatabaseOrOrdinaryStorageOwners(t *testing.T) {
	p, req := backupFixture(t)
	stack, _ := p.LoadStack()
	stack.Services = map[string]manifest.Service{"database": {Runtime: manifest.RuntimeContainer, Image: "example", Mounts: []string{"bind://./data/pgdata:/data"}}, "files": {Runtime: manifest.RuntimeContainer, Image: "example", Mounts: []string{"bind://./data:/data"}}}
	stack.Jobs = map[string]manifest.Job{"capture-database": {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-dump"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "one-owned-db", Output: "database.dump", ExcludePersist: []string{"pgdata"}}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection.ServiceState = api.BackupGroupSelection{Mode: "all"}
	preview, err := p.BackupPreview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range preview.Resources {
		if resource.Group == "service_state" {
			t.Fatalf("duplicate persistence owner %+v", resource)
		}
	}
}

func TestBackupVerificationRejectsValidZIPWithChangedPayload(t *testing.T) {
	original := []byte("good")
	sum := sha256.Sum256(original)
	manifest := api.BackupManifest{Version: 1, Entries: []api.BackupEntry{{Path: "payload", SizeBytes: int64(len(original)), SHA256: hex.EncodeToString(sum[:]), Mode: 0600}}}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"good", "evil", "mode"} {
		t.Run(data, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := zip.NewWriter(&buffer)
			header := &zip.FileHeader{Name: "payload"}
			header.SetMode(0600)
			if data == "mode" {
				header.SetMode(0644)
			}
			entry, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			content := data
			if data == "mode" {
				content = "good"
			}
			if _, err = entry.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
			entry, err = writer.Create("manifest.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = entry.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			err = verifyBackupZIP(t.Context(), bytes.NewReader(buffer.Bytes()), int64(buffer.Len()), manifest, payload)
			if data == "good" && err != nil {
				t.Fatal(err)
			}
			if data != "good" && err == nil {
				t.Fatal("accepted a valid ZIP with changed payload and valid CRC")
			}
		})
	}
}

func TestBackupConfigurationOnlyPreservesRecovery(t *testing.T) {
	p, req := backupFixture(t)
	req.Selection = api.BackupSelection{Version: 1}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("%+v", op)
	}
	if len(op.Artifact.Manifest.Entries) != 2 {
		t.Fatalf("entries=%+v", op.Artifact.Manifest.Entries)
	}
	for _, entry := range op.Artifact.Manifest.Entries {
		if entry.Path != "recovery/angee.yaml" && entry.Path != "recovery/secrets.env" {
			t.Fatalf("unexpected entry %+v", entry)
		}
	}
}
func TestBackupPreviewReasonsDoNotExposeAbsolutePaths(t *testing.T) {
	p, req := backupFixture(t)
	if err := os.RemoveAll(filepath.Join(p.root, "data")); err != nil {
		t.Fatal(err)
	}
	preview, err := p.BackupPreview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range preview.Resources {
		if strings.Contains(resource.Reason, p.root) || strings.Contains(resource.Reason, "/Users/") {
			t.Fatalf("unsafe reason %q", resource.Reason)
		}
	}
}

func TestBackupTraversalRejectsDirectoryReplacementAndContainedAncestorLinks(t *testing.T) {
	for _, kind := range []string{"ancestor", "during_visit"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			allowed := filepath.Join(root, "allowed")
			physical := filepath.Join(root, "pgdata")
			for _, directory := range []string{allowed, physical} {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(physical, "physical"), []byte("excluded"), 0600); err != nil {
				t.Fatal(err)
			}
			tree := backupTree{scope: root, root: root, prefix: "storage", exclude: []string{physical}}
			if kind == "ancestor" {
				if err := os.Symlink(physical, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
				tree.root = filepath.Join(root, "alias")
			}
			copied := false
			err := walkBackupTree(t.Context(), tree, func(_ *os.Root, name string, info fs.FileInfo, _ string) error {
				if name == "allowed" && info.IsDir() {
					if err := os.Rename(allowed, filepath.Join(root, "renamed")); err != nil {
						return err
					}
					return os.Symlink(physical, allowed)
				}
				if name == "physical" {
					copied = true
				}
				return nil
			}, nil)
			if err == nil || copied {
				t.Fatalf("err=%v copied physical state=%v", err, copied)
			}
		})
	}
}

// A file deleted after its directory was listed is not part of a live
// capture, whether it goes before the walk stats it or before the archive opens
// or reads it as a link, and leaves nothing in the archive. In repository
// metadata, where a repack deletes the pack it replaced, the deletion fails the
// walk instead. A required entry that goes fails too.
func TestBackupSkipsFilesRemovedAfterListing(t *testing.T) {
	for _, repository := range []bool{false, true} {
		for _, window := range []string{"before stat", "before open", "before readlink"} {
			if repository && window == "before readlink" {
				continue // a repository tree refuses a symlink before reading it
			}
			t.Run(fmt.Sprintf("%s repository=%v", window, repository), func(t *testing.T) {
				root := t.TempDir()
				for _, name := range []string{"a", "b", "c"} {
					if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if window == "before readlink" {
					if err := os.Remove(filepath.Join(root, "b")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
						t.Fatal(err)
					}
				}
				var buffer bytes.Buffer
				writer := zip.NewWriter(&buffer)
				manifest := api.BackupManifest{Entries: []api.BackupEntry{}}
				archive := &backupArchive{ctx: t.Context(), writer: writer, manifest: &manifest, names: map[string]bool{}}
				tree := backupTree{scope: root, root: root, prefix: "storage", repository: repository}
				err := walkBackupTree(t.Context(), tree, func(source *os.Root, name string, info fs.FileInfo, archiveName string) error {
					// The walk lists a, b and c at once, then stats and visits each in turn.
					if window == "before stat" && name == "a" || window != "before stat" && name == "b" {
						if err := os.Remove(filepath.Join(root, "b")); err != nil {
							return err
						}
					}
					return archive.add(source, name, info, archiveName)
				}, nil)
				if repository {
					if err == nil {
						t.Fatal("walk of repository metadata skipped a removed file; want it failed")
					}
					return
				}
				if err != nil {
					t.Fatalf("walk = %v, want the removed file skipped", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				var declared []string
				for _, entry := range manifest.Entries {
					declared = append(declared, entry.Path)
				}
				reader, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
				if err != nil {
					t.Fatal(err)
				}
				var written []string
				for _, entry := range reader.File {
					written = append(written, strings.TrimSuffix(entry.Name, "/"))
				}
				want := []string{"storage", "storage/a", "storage/c"}
				if !slices.Equal(declared, want) || !slices.Equal(written, want) {
					t.Fatalf("declared %q and wrote %q, want %q", declared, written, want)
				}
				info, err := os.Lstat(filepath.Join(root, "a"))
				if err != nil {
					t.Fatal(err)
				}
				source, err := os.OpenRoot(root)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = source.Close() }()
				if err := os.Remove(filepath.Join(root, "a")); err != nil {
					t.Fatal(err)
				}
				if err := archive.add(source, "a", info, "recovery/a"); !errors.Is(err, errBackupEntryGone) {
					t.Fatalf("required entry removed before it was opened: add = %v, want errBackupEntryGone", err)
				}
			})
		}
	}
}

// Only repository metadata trees refuse symlinks. A source named repository
// still keeps its worktree's links, though its archive prefix holds
// "/repository/".
func TestBackupSourceNamedRepositoryKeepsWorktreeSymlinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	p, req := backupFixture(t)
	source := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("tracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README", filepath.Join(source, "LINK")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.name", "backup"}, {"config", "user.email", "backup@example.invalid"}, {"add", "."}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = source
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Sources = map[string]manifest.Source{"repository": {Kind: "local", Path: source}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection = api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "all"}, Policy: "bundle"}}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded || op.Artifact == nil {
		t.Fatalf("backup of a source named repository = %+v", op)
	}
	link := "sources/stack/repository/worktree/LINK"
	if !slices.ContainsFunc(op.Artifact.Manifest.Entries, func(entry api.BackupEntry) bool {
		return entry.Path == link && fs.FileMode(entry.Mode)&fs.ModeSymlink != 0
	}) {
		t.Fatalf("archive lacks the worktree symlink %s", link)
	}
}

func TestBackupRepositoryComponentsRetainSafetyExclusions(t *testing.T) {
	p, req := backupFixture(t)
	metadata := filepath.Join(p.root, "backups", "gitdata")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "--separate-git-dir", metadata, p.root)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	for _, args := range [][]string{{"config", "user.name", "backup"}, {"config", "user.email", "backup@example.invalid"}, {"add", "angee.yaml"}, {"commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = p.root
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Sources = map[string]manifest.Source{"self": {Kind: "local", Path: p.root}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection = api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "all"}}}
	preview, err := p.BackupPreview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range preview.Resources {
		if resource.Group == "sources" && resource.Name == "stack:self" {
			if resource.Status != "unsupported" {
				t.Fatalf("metadata safety root accepted: %+v", resource)
			}
			return
		}
	}
	t.Fatal("source not discovered")
}

type unsupportedCaptureBackend struct{ stubStatusBackend }

func (unsupportedCaptureBackend) ValidateJob(runtime.JobSpec) error {
	return &runtime.JobCapabilityError{Reason: "native transient capture unavailable"}
}
func TestBackupDiscoveryHonorsNativeJobCapability(t *testing.T) {
	p, req := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"capture": {Runtime: manifest.RuntimeLocal, Command: []string{"must-not-run"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	p.procBackend = unsupportedCaptureBackend{}
	req.Selection = api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}
	preview, err := p.BackupPreview(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range preview.Resources {
		if resource.Group == "databases" {
			if resource.Status != "unsupported" || resource.Reason != "native transient capture unavailable" {
				t.Fatalf("%+v", resource)
			}
			return
		}
	}
	t.Fatal("capture capability missing")
}

func TestBackupPackedGitHistoryRetainsEmptyDirectoryRoots(t *testing.T) {
	p, req := backupFixture(t)
	source := filepath.Join(p.root, "source")
	if err := os.MkdirAll(filepath.Join(source, "empty-worktree-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(source, "init")
	run(source, "config", "user.name", "backup")
	run(source, "config", "user.email", "backup@example.invalid")
	run(source, "commit", "--allow-empty", "-m", "initial")
	run(source, "pack-refs", "--all", "--prune")
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Sources = map[string]manifest.Source{"packed": {Kind: "local", Path: source}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection = api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "all"}}}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("%+v", op)
	}
	archive, err := zip.OpenReader(filepath.Join(p.root, op.Artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	restored := t.TempDir()
	for _, entry := range archive.File {
		relative := ""
		for from, to := range map[string]string{"sources/stack/packed/worktree/": "", "sources/stack/packed/repository/git/": ".git/"} {
			if strings.HasPrefix(entry.Name, from) {
				relative = to + strings.TrimPrefix(entry.Name, from)
				break
			}
		}
		if relative == "" {
			continue
		}
		destination := filepath.Join(restored, filepath.FromSlash(relative))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, entry.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			t.Fatal(err)
		}
		content, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(content)
		content.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, data, entry.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range []string{".git/refs", "empty-worktree-directory"} {
		info, err := os.Stat(filepath.Join(restored, relative))
		if err != nil || !info.IsDir() {
			t.Fatalf("missing directory %q: %v", relative, err)
		}
	}
	run(restored, "fsck", "--full")
	run(restored, "rev-parse", "--verify", "HEAD")
}

type failedDiagnosticCaptureBackend struct{ stubStatusBackend }

func (failedDiagnosticCaptureBackend) RunJob(context.Context, runtime.Target, runtime.JobSpec) ([]byte, error) {
	return []byte("capture failed with fixture-sensitive-secret"), errors.New("native exit status 23: fixture-sensitive-secret")
}
func TestBackupFailureRetainsBoundedRedactedNativeDiagnostics(t *testing.T) {
	p, req := backupFixture(t)
	logctx.RegisterSecrets("fixture-sensitive-secret")
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"capture": {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-capture"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	p.procBackend = failedDiagnosticCaptureBackend{}
	req.Selection = api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunFailed || !strings.Contains(op.Output, "capture failed") || !strings.Contains(op.Nodes[0].Message, "exit status 23") {
		t.Fatalf("%+v", op)
	}
	data, _ := json.Marshal(op)
	if strings.Contains(string(data), "fixture-sensitive-secret") {
		t.Fatal("native diagnostics leaked registered secret")
	}
}

// printingCaptureBackend runs every capture job: it writes the job's dump into
// the backup's output directory for it and prints a line naming the job.
type printingCaptureBackend struct {
	stubStatusBackend
	captures string
}

func (b *printingCaptureBackend) RunJob(_ context.Context, _ runtime.Target, spec runtime.JobSpec) ([]byte, error) {
	if err := os.WriteFile(filepath.Join(b.captures, spec.Name, "app.dump"), []byte("dump\n"), 0600); err != nil {
		return nil, err
	}
	return []byte("dumped by " + spec.Name), nil
}

// A backup receipt's output keeps every capture's output, each line labelled
// with its capture's group and name, not only the last capture's.
func TestBackupReceiptKeepsEveryCaptureOutput(t *testing.T) {
	p, req := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{
		"first":  {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-dump"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"}},
		"second": {Runtime: manifest.RuntimeLocal, Command: []string{"trusted-dump"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "audit", Output: "app.dump"}},
	}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	p.procBackend = &printingCaptureBackend{captures: filepath.Join(p.root, "state/backups", req.RequestID, "captures")}
	req.Selection = api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if want := "databases/app | dumped by first\ndatabases/audit | dumped by second\n"; op.Status != api.JobRunSucceeded || op.Output != want {
		t.Fatalf("receipt status %s, output %q; want it succeeded with output %q", op.Status, op.Output, want)
	}
}

func TestBackupNativeCaptureUsesCompiledEnvironmentAndLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("native process-compose integration")
	}
	if _, err := exec.LookPath("process-compose"); err != nil {
		t.Skip("process-compose unavailable")
	}
	p, req := backupFixture(t)
	if validator, ok := p.procBackend.(runtime.JobValidator); ok {
		if err := validator.ValidateJob(runtime.JobSpec{Transient: true}); err != nil {
			t.Skip(err)
		}
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"capture": {Runtime: manifest.RuntimeLocal, Workdir: ".", Command: []string{"sh", "-eu", "-c", `printf 'native compiled payload' > "$BACKUP_OUTPUT/database.dump"; printf compiled-capture-ok`}, Env: map[string]string{"BACKUP_OUTPUT": "${inputs.backup_output_absolute}"}, Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "database.dump"}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection = api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded {
		t.Fatalf("native compiled capture: %+v", op)
	}
}

func TestBackupPreviewBudgetKeepsResourcesAndNeverPublishesPartialSizes(t *testing.T) {
	p, req := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Persist["later"] = manifest.PersistPath{Subpath: "./later", Scope: "stack"}
	if err := os.Mkdir(filepath.Join(p.root, "later"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	complete, _, err := p.backupDiscover(t.Context(), req, &backupEstimateBudget{remaining: 100})
	if err != nil {
		t.Fatal(err)
	}
	bounded, _, err := p.backupDiscover(t.Context(), req, &backupEstimateBudget{remaining: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != len(bounded) {
		t.Fatalf("bounded preview lost resources: %d != %d", len(complete), len(bounded))
	}
	for i, resource := range bounded {
		if resource.Name != complete[i].Name || resource.Status != complete[i].Status {
			t.Fatalf("selection changed: %+v vs %+v", resource, complete[i])
		}
		if resource.Status == "included" && resource.Group == "storage" {
			if resource.EstimatedBytes != nil || resource.Reason == "" {
				t.Fatalf("partial count published as complete: %+v", resource)
			}
		}
	}
	fair, _, err := p.backupDiscover(t.Context(), req, &backupEstimateBudget{remaining: 100, perResource: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range fair {
		if resource.Name == "app-data" && resource.EstimatedBytes != nil {
			t.Fatalf("large resource escaped per-resource budget: %+v", resource)
		}
		if resource.Name == "later" && (resource.EstimatedBytes == nil || *resource.EstimatedBytes != 0) {
			t.Fatalf("large resource starved later small estimate: %+v", resource)
		}
	}

}

func TestBackupDiscoveryCallerCancellationIsNotCapabilityFailure(t *testing.T) {
	p, req := backupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.BackupPreview(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation=%v", err)
	}
	resource := backupResource{BackupResource: api.BackupResource{Status: "included"}, trees: []backupTree{{scope: p.root, root: filepath.Join(p.root, "data"), prefix: "storage"}}}
	if err := resource.inspect(ctx, &backupEstimateBudget{}); !errors.Is(err, context.Canceled) || resource.Status != "included" {
		t.Fatalf("cancellation became unsupported: %+v %v", resource, err)
	}
}

func TestBackupCapturePreflightStillRejectsUnsafeRootsWithoutEstimating(t *testing.T) {
	p, req := backupFixture(t)
	alias := filepath.Join(p.root, "alias")
	if err := os.Symlink(filepath.Join(p.root, "data", "pgdata"), alias); err != nil {
		t.Fatal(err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Persist["unsafe-alias"] = manifest.PersistPath{Subpath: "./alias", Scope: "stack"}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []*backupEstimateBudget{nil, {remaining: 0}} {
		resources, _, err := p.backupDiscover(t.Context(), req, budget)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, resource := range resources {
			if resource.Name == "unsafe-alias" {
				found = true
				if resource.Status != "unsupported" {
					t.Fatalf("unsafe root accepted: %+v", resource)
				}
			}
		}
		if !found {
			t.Fatal("unsafe root omitted")
		}
	}
}

func TestBackupCaptureStillFullyValidatesMetadataAfterBoundedDiscovery(t *testing.T) {
	p, req := backupFixture(t)
	source := filepath.Join(p.root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"config", "user.name", "backup"}, {"config", "user.email", "backup@example.invalid"}, {"commit", "--allow-empty", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = source
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	outside := filepath.Join(t.TempDir(), "external-secret")
	if err := os.WriteFile(outside, []byte("must-not-be-read"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, ".git", "unsafe-metadata-link")); err != nil {
		t.Fatal(err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Sources = map[string]manifest.Source{"app": {Kind: "local", Path: source}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req.Selection = api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "all"}}}
	resources, _, err := p.backupDiscover(t.Context(), req, &backupEstimateBudget{})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if resource.Group == "sources" && (resource.Status != "included" || resource.EstimatedBytes != nil) {
			t.Fatalf("bounded discovery=%+v", resource)
		}
	}
	if _, err := p.BackupStart(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunFailed || op.Artifact != nil || !strings.Contains(op.Error, "metadata symlink") {
		t.Fatalf("unsafe metadata published after preflight: %+v", op)
	}
}

func TestBackupPreviewBudgetCountsEnumerationBeforeExclusions(t *testing.T) {
	root := t.TempDir()
	tree := backupTree{scope: root, root: root, prefix: "storage"}
	for i := 0; i < 12; i++ {
		filename := filepath.Join(root, string(rune('a'+i)))
		if err := os.WriteFile(filename, []byte("excluded"), 0600); err != nil {
			t.Fatal(err)
		}
		tree.exclude = append(tree.exclude, filename)
	}
	request := &backupEstimateBudget{remaining: 5}
	visits := 0
	err := walkBackupTree(t.Context(), tree, func(*os.Root, string, fs.FileInfo, string) error { visits++; return nil }, &backupScanBudget{request: request, remaining: 5})
	if !errors.Is(err, errBackupEstimateIncomplete) || request.remaining != 0 || visits != 1 {
		t.Fatalf("enumeration was not bounded before exclusions: visits=%d remaining=%d err=%v", visits, request.remaining, err)
	}
}

func TestBackupPreviewBudgetCannotHideUnsafeLaterComponentRoot(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	excluded := filepath.Join(root, "pgdata")
	for _, directory := range []string{allowed, excluded} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(allowed, "payload"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	resource := backupResource{BackupResource: api.BackupResource{Status: "included"}, trees: []backupTree{{scope: root, root: allowed, prefix: "source/worktree"}, {scope: root, root: excluded, prefix: "source/repository/git", exclude: []string{excluded}}}}
	if err := resource.inspect(t.Context(), &backupEstimateBudget{remaining: 1}); err != nil {
		t.Fatal(err)
	}
	if resource.Status != "unsupported" {
		t.Fatalf("estimate cap hid unsafe repository root: %+v", resource)
	}
}

// TestBackupFetchPublishedColocatedJJ captures a real colocated jj store and a
// workspace of it from their remote alone. Fully published, both pass, though
// each carries jj's ignored .jj tree and refs/jj/ keep refs. A dirty working
// copy (before and after jj snapshots it), a commit only jj can see, and an
// ignored file outside .jj are each refused, and passing returns once each is
// gone. Git's reflogs still count: a commit rewritten after jj exported it as
// HEAD is refused, though jj hides it.
func TestBackupFetchPublishedColocatedJJ(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	home := t.TempDir()
	for key, value := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "JJ_CONFIG": filepath.Join(home, "jj.toml"), "GIT_CONFIG_GLOBAL": filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM": "1", "JJ_USER": "backup-test", "JJ_EMAIL": "backup@example.invalid", "GIT_AUTHOR_NAME": "backup-test", "GIT_AUTHOR_EMAIL": "backup@example.invalid", "GIT_COMMITTER_NAME": "backup-test", "GIT_COMMITTER_EMAIL": "backup@example.invalid"} {
		t.Setenv(key, value)
	}
	if err := jjdriver.Probe(t.Context()).Err(); err != nil {
		if os.Getenv("ANGEE_REQUIRE_JJ") == "1" {
			t.Fatalf("ANGEE_REQUIRE_JJ=1, but %v", err)
		}
		t.Skip(err)
	}
	run := func(dir, tool string, args ...string) {
		t.Helper()
		cmd := exec.Command(tool, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v %s", tool, args, err, out)
		}
	}
	write := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	remote, seed, primary := filepath.Join(base, "remote.git"), filepath.Join(base, "seed"), filepath.Join(base, "primary")
	run(base, "git", "init", "-q", "--bare", "-b", "main", remote)
	run(base, "git", "clone", "-q", remote, seed)
	write(filepath.Join(seed, "tracked"), "published\n")
	run(seed, "git", "add", ".")
	run(seed, "git", "commit", "-q", "-m", "published")
	run(seed, "git", "push", "-q", "origin", "HEAD:main")
	run(base, "git", "clone", "-q", remote, primary)
	run(primary, "jj", "git", "init", "--colocate")
	p, _ := backupFixture(t)
	slot := filepath.Join(p.root, "workspaces/dev/work")
	if err := os.MkdirAll(filepath.Dir(slot), 0700); err != nil {
		t.Fatal(err)
	}
	run(primary, "jj", "workspace", "add", "--colocate", "--name", "dev", "-r", "main@origin", slot)
	stack, _ := p.LoadStack()
	stack.Sources = map[string]manifest.Source{"work": {Kind: "local", Path: primary}}
	stack.Workspaces = map[string]manifest.Workspace{"dev": {Template: "test", Sources: map[string]manifest.WorkspaceSource{"work": {Source: "work", Subpath: "work"}}}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req := api.BackupRequest{Version: 1, RequestID: "published-jj", Selection: api.BackupSelection{Sources: api.BackupSourceSelection{BackupGroupSelection: api.BackupGroupSelection{Mode: "selected", Names: []string{"stack:work", "workspace:dev/work"}}, Policy: "fetch_published"}}}
	verdicts := func() map[string]api.BackupResource {
		t.Helper()
		preview, err := p.BackupPreview(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]api.BackupResource{}
		for _, resource := range preview.Resources {
			found[resource.Name] = resource
		}
		return found
	}
	published := func(name string) {
		t.Helper()
		found := verdicts()
		for _, resource := range []string{"stack:work", "workspace:dev/work"} {
			if got := found[resource]; got.Status != "included" {
				t.Fatalf("%s: %s=%+v, want it captured from its remote", name, resource, got)
			}
		}
		_, evidence, err := p.backupSourceTrees(context.Background(), "workspace:dev/work", slot, primary, nil, "fetch_published")
		if err != nil || evidence.Policy != "fetch_published" || evidence.Remotes["origin"] == "" || evidence.GitRevision == "" || evidence.JJRevision == "" || evidence.JJOperation == "" {
			t.Fatalf("%s: evidence=%+v err=%v", name, evidence, err)
		}
	}
	refused := func(name string) {
		t.Helper()
		if got := verdicts()["workspace:dev/work"]; got.Status != "unsupported" || !strings.HasPrefix(got.Reason, "remote-only capture refuses") {
			t.Fatalf("%s: workspace:dev/work=%+v, want a publication refusal", name, got)
		}
		var refusal *gitx.BackupPublicationError
		if _, _, err := p.backupSourceTrees(context.Background(), "workspace:dev/work", slot, primary, nil, "fetch_published"); !errors.As(err, &refusal) {
			t.Fatalf("%s: err=%v, want a publication refusal", name, err)
		}
	}

	published("fully published")

	write(filepath.Join(slot, "dirty.txt"), "unsnapshotted edit\n")
	refused("edit jj has not snapshotted")
	run(slot, "jj", "status")
	refused("snapshotted dirty working copy")
	run(slot, "jj", "restore")
	published("restored working copy")

	write(filepath.Join(slot, "side.txt"), "jj-only work\n")
	run(slot, "jj", "new", "main@origin")
	refused("commit no Git ref holds")
	run(slot, "jj", "abandon", "-r", "visible_heads() ~ working_copies()")
	published("abandoned side commit")

	write(filepath.Join(primary, ".git/info/exclude"), "build/\n")
	write(filepath.Join(slot, "build/out"), "stray ignored output\n")
	refused("ignored file outside .jj")
	if err := os.RemoveAll(filepath.Join(slot, "build")); err != nil {
		t.Fatal(err)
	}
	published("stray file removed")

	write(filepath.Join(slot, "rewritten.txt"), "rewritten before it was pushed\n")
	run(slot, "jj", "commit", "-m", "first draft")
	run(slot, "jj", "describe", "-r", "@-", "-m", "final")
	run(slot, "git", "push", "-q", "origin", "HEAD:main")
	run(slot, "jj", "git", "fetch")
	run(slot, "jj", "bookmark", "set", "main", "-r", "main@origin")
	if got := verdicts()["workspace:dev/work"]; got.Status != "unsupported" || got.Reason != "remote-only capture refuses unpublished reflog history; use the bundle source policy" {
		t.Fatalf("draft held only by HEAD's reflog: workspace:dev/work=%+v, want the reflog refusal", got)
	}
}

// backupPlanningDriver is a slot driver with the capture capability: it
// returns plan without running anything and records what the service asked.
type backupPlanningDriver struct {
	*fakeDriver
	plan       vcs.BackupPlan
	planned    []vcs.Slot
	remoteOnly []bool
	layers     []vcs.BackupLayer
}

func (d *backupPlanningDriver) BackupPlan(_ context.Context, s vcs.Slot, remoteOnly bool, layer vcs.BackupLayer) (vcs.BackupPlan, error) {
	d.planned = append(d.planned, s)
	d.remoteOnly = append(d.remoteOnly, remoteOnly)
	d.layers = append(d.layers, layer)
	return d.plan, nil
}

// fakeBackupLayer is the layer a test hands the service; only its identity is
// compared, so planning through it is never expected. name keeps the type
// non-zero-sized, so two pointers to it never compare equal by accident.
type fakeBackupLayer struct{ name string }

func (*fakeBackupLayer) BackupLayerPlan(context.Context, vcs.Slot, string, bool) (vcs.BackupPlan, error) {
	return vcs.BackupPlan{}, errors.New("the fake slot driver plans no layer")
}

// TestBackupSourcePlansThroughDriverSeams injects the slot driver and the
// colocated layer through their Platform seams: a source capture asks the
// slot's driver to plan, hands it the seam's layer, and copies exactly the
// plan's components, while a driver without the capability is refused.
func TestBackupSourcePlansThroughDriverSeams(t *testing.T) {
	p, _ := backupFixture(t)
	source := filepath.Join(p.root, "sources/app")
	metadata := filepath.Join(source, ".fake-vcs")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	driver := &backupPlanningDriver{fakeDriver: &fakeDriver{}, plan: vcs.BackupPlan{
		Components:      []vcs.BackupComponent{{Name: "fake", Root: metadata}},
		WorktreeExclude: []string{metadata},
		Evidence:        api.BackupSourceEvidence{GitRevision: "fake-revision", Policy: "bundle"},
	}}
	layer := &fakeBackupLayer{name: "seam"}
	var layered []vcs.Slot
	p.slotDrivers = func(vcs.Slot) vcs.Driver { return driver }
	p.backupLayers = func(s vcs.Slot) vcs.BackupLayer { layered = append(layered, s); return layer }

	trees, evidence, err := p.backupSourceTrees(context.Background(), "stack:app", source, source, nil, "bundle")
	if err != nil {
		t.Fatal(err)
	}
	want := vcs.Slot{Path: source, Primary: source}
	if len(driver.planned) != 1 || driver.planned[0] != want || driver.remoteOnly[0] || driver.layers[0] != vcs.BackupLayer(layer) || len(layered) != 1 || layered[0] != want {
		t.Fatalf("planned=%+v remoteOnly=%v layers=%v layered=%+v", driver.planned, driver.remoteOnly, driver.layers, layered)
	}
	if evidence.GitRevision != "fake-revision" || len(trees) != 2 {
		t.Fatalf("evidence=%+v trees=%+v", evidence, trees)
	}
	if worktree := trees[0]; worktree.root != source || worktree.prefix != "sources/stack/app/worktree" || !slices.Contains(worktree.exclude, metadata) {
		t.Fatalf("worktree tree=%+v", worktree)
	}
	if component := trees[1]; component.root != metadata || component.prefix != "sources/stack/app/repository/fake" {
		t.Fatalf("component tree=%+v", component)
	}
	if _, evidence, err := p.backupSourceTrees(context.Background(), "stack:app", source, source, nil, "fetch_published"); err != nil || evidence.GitRevision != "fake-revision" || !driver.remoteOnly[1] {
		t.Fatalf("remote-only plan: evidence=%+v err=%v remoteOnly=%v", evidence, err, driver.remoteOnly)
	}

	p.slotDrivers = func(vcs.Slot) vcs.Driver { return &fakeDriver{} }
	var refusal *vcs.BackupCapabilityError
	if _, _, err := p.backupSourceTrees(context.Background(), "stack:app", source, source, nil, "bundle"); !errors.As(err, &refusal) {
		t.Fatalf("a driver without the capture capability: err=%v", err)
	}
}

// captureRecorder is a container backend that records the capture job it is
// asked to run and writes the dump that job would have, into the backup's
// output directory for it.
type captureRecorder struct {
	stubStatusBackend
	output string
	spec   runtime.JobSpec
}

func (b *captureRecorder) RunJob(_ context.Context, _ runtime.Target, spec runtime.JobSpec) ([]byte, error) {
	b.spec = spec
	return nil, os.WriteFile(filepath.Join(b.output, "app.dump"), []byte("dump\n"), 0o600)
}

// A container capture job reaches the container backend as a transient job,
// compiled with the output directory the backup chose for it, and its dump is
// archived under its capture's group and name.
func TestContainerCaptureRunsTransientWithItsBackupOutput(t *testing.T) {
	p, _ := backupFixture(t)
	stack, err := p.LoadStack()
	if err != nil {
		t.Fatal(err)
	}
	stack.Jobs = map[string]manifest.Job{"dump": {
		Runtime: manifest.RuntimeContainer, Image: "alpine:3",
		Mounts:  []string{"bind://./${inputs.backup_output}:/out"},
		Command: []string{"sh", "-c", "echo dump > /out/app.dump"},
		Capture: &manifest.BackupCapture{Group: "databases", Name: "app", Output: "app.dump"},
	}}
	if err := manifest.SaveFile(manifest.Path(p.root), stack); err != nil {
		t.Fatal(err)
	}
	req := api.BackupRequest{Version: 1, RequestID: "container-capture", Selection: api.BackupSelection{Databases: api.BackupGroupSelection{Mode: "all"}}}
	output := "state/backups/" + req.RequestID + "/captures/dump"
	recorder := &captureRecorder{output: filepath.Join(p.root, output)}
	p.composeBackend = recorder

	if _, err := p.BackupStart(context.Background(), req); err != nil {
		t.Fatalf("BackupStart() error = %v", err)
	}
	op := waitBackup(t, p, req.RequestID)
	if op.Status != api.JobRunSucceeded || op.Artifact == nil {
		t.Fatalf("backup = %+v, want it succeeded with an artifact", op)
	}
	if recorder.spec.Name != "dump" || !recorder.spec.Transient || !strings.Contains(string(recorder.spec.Configuration), output) {
		t.Fatalf("container backend ran %q (transient %v) with configuration\n%s\nwant the transient capture job bound to %s", recorder.spec.Name, recorder.spec.Transient, recorder.spec.Configuration, output)
	}
	archive, err := zip.OpenReader(filepath.Join(p.root, op.Artifact.Path))
	if err != nil {
		t.Fatalf("OpenReader() error = %v", err)
	}
	defer func() { _ = archive.Close() }()
	if !slices.ContainsFunc(archive.File, func(f *zip.File) bool { return f.Name == "captures/databases/app/app.dump" }) {
		t.Fatalf("archive lacks captures/databases/app/app.dump")
	}
}
