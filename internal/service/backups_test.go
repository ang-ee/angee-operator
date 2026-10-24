package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
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
			})
			if err == nil || copied {
				t.Fatalf("err=%v copied physical state=%v", err, copied)
			}
		})
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
