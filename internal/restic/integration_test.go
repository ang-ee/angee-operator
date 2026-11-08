package restic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireRestic(t *testing.T) *Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("restic repository operations require inherited password descriptors")
	}
	required := os.Getenv("ANGEE_REQUIRE_RESTIC") == "1"
	if testing.Short() {
		if required {
			t.Fatal("ANGEE_REQUIRE_RESTIC=1, but restic integration tests cannot run under -short")
		}
		t.Skip("restic integration test skipped under -short")
	}
	client := New()
	path, err := client.ResolveBinary(t.Context())
	if err != nil {
		if required {
			t.Fatalf("ANGEE_REQUIRE_RESTIC=1, but %v", err)
		}
		t.Skipf("restic is unavailable: %v", err)
	}
	client.Binary = path
	version, err := client.Version(t.Context())
	if err != nil {
		if required {
			t.Fatalf("ANGEE_REQUIRE_RESTIC=1, but %v", err)
		}
		t.Skipf("restic is unsupported: %v", err)
	}
	if version.Version != MinVersion && !VersionAtLeast(version.Version, MinVersion) {
		t.Fatalf("restic version = %q", version.Version)
	}
	return client
}

func newRealRepository(t *testing.T, client *Client, name string) Repository {
	t.Helper()
	root := t.TempDir()
	repository := Repository{
		URL:      filepath.Join(root, name),
		Password: "test-password-" + name,
		CacheDir: filepath.Join(root, "cache-"+name),
	}
	if _, err := client.Init(t.Context(), InitOptions{Repository: repository}); err != nil {
		t.Fatalf("Init(%s): %v", name, err)
	}
	return repository
}

func TestRealNestedStdinDumpRoundTrip(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "nested")
	payload := []byte("postgres archive bytes\x00with binary data")
	backup, err := client.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: repository,
		Filename:   "databases/app/app.pgdump",
		Tags:       []string{"angee.snapshot=s1", "angee.kind=member"},
		Host:       "stack-a",
	}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var dumped bytes.Buffer
	if err := client.Dump(t.Context(), DumpOptions{
		Repository: repository,
		SnapshotID: backup.SnapshotID,
		Path:       "/databases/app/app.pgdump",
	}, &dumped); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dumped.Bytes(), payload) {
		t.Fatalf("dump = %q, want %q", dumped.Bytes(), payload)
	}
}

func TestRealStdinFailuresReturnKindsAndPartialID(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "stdin-failures")
	result, err := client.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: repository,
		Filename:   "databases/app/empty.dump",
	}, strings.NewReader(""))
	if err == nil || !IsKind(err, WriteFailure) {
		t.Fatalf("empty BackupStdin() = %+v, %v; want WriteFailure", result, err)
	}
	if result.SnapshotID == "" {
		t.Fatalf("empty BackupStdin() returned no partial snapshot ID: %v", err)
	}
	before, err := client.Snapshots(t.Context(), SnapshotsOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}

	producerErr := errors.New("producer failed after bytes")
	result, err = client.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: repository,
		Filename:   "databases/app/partial.dump",
	}, &dataErrorReader{data: []byte("partial payload"), err: producerErr})
	if result.SnapshotID != "" || err == nil || !errors.Is(err, producerErr) {
		t.Fatalf("reader-error BackupStdin() = %+v, %v", result, err)
	}
	after, err := client.Snapshots(t.Context(), SnapshotsOptions{Repository: repository})
	if err != nil || len(after) != len(before) {
		t.Fatalf("reader error committed a snapshot: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestRealTagFiltersAndUnknownForget(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "filters")
	for index, tags := range [][]string{{"a", "b"}, {"a"}, {"c"}} {
		_, err := client.BackupStdin(t.Context(), BackupStdinOptions{
			Repository: repository,
			Filename:   "stack/member-" + string(rune('a'+index)),
			Tags:       tags,
			Host:       "stack-a",
		}, strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := client.Snapshots(t.Context(), SnapshotsOptions{
		Repository: repository,
		Filter:     SnapshotFilter{Tags: [][]string{{"a", "b"}, {"c"}}, Host: "stack-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("filtered snapshots = %d, want 2: %+v", len(snapshots), snapshots)
	}
	unknown := strings.Repeat("f", 64)
	forget, err := client.Forget(t.Context(), ForgetOptions{Repository: repository, IDs: []string{unknown}})
	if err != nil || !strings.Contains(forget.Output, "Ignoring") {
		t.Fatalf("Forget(unknown) = %q, %v", forget.Output, err)
	}
	if _, err := client.Forget(t.Context(), ForgetOptions{Repository: repository, IDs: []string{snapshots[0].ID[:8]}}); err == nil {
		t.Fatal("Forget accepted an 8-character snapshot prefix")
	}
}

func TestRealWrongPasswordAndUnknownSnapshotKinds(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "error-kinds")
	wrong := repository
	wrong.Password = "wrong-password"
	if _, err := client.Snapshots(t.Context(), SnapshotsOptions{Repository: wrong}); !IsKind(err, WrongPassword) {
		t.Fatalf("wrong-password error = %v", err)
	}
	if err := client.Dump(t.Context(), DumpOptions{
		Repository: repository,
		SnapshotID: strings.Repeat("d", 64),
		Path:       "/missing",
	}, io.Discard); !IsKind(err, NotFound) {
		t.Fatalf("unknown-snapshot error = %v", err)
	}
}

func TestRealWrongPasswordReturnsWhileReaderBlocked(t *testing.T) {
	t.Parallel()
	c := requireRestic(t)
	repository := newRealRepository(t, c, "blocked-wrong-password")
	repository.Password = "definitely-wrong-password"
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), closeErr: errors.New("reader closed")}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.BackupStdin(ctx, BackupStdinOptions{Repository: repository, Filename: "stack/member"}, reader)
	if !IsKind(err, WrongPassword) {
		t.Fatalf("BackupStdin() error = %v, want WrongPassword", err)
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("wrong-password BackupStdin took %s with a blocked reader", elapsed)
	}
}

func TestRealCancelBackupLeavesNoStaleLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix SIGTERM lock cleanup")
	}
	c := requireRestic(t)
	repository := newRealRepository(t, c, "cancel-clean-lock")
	reader := newStagedBlockingReader(errors.New("reader closed"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.BackupStdin(ctx, BackupStdinOptions{Repository: repository, Filename: "stack/member"}, reader)
		done <- err
	}()
	<-reader.started
	locks, err := listRealLocks(t.Context(), c, repository)
	if err != nil || strings.TrimSpace(string(locks)) == "" {
		cancel()
		<-done
		t.Fatalf("backup held no lock before cancellation: %q, %v", locks, err)
	}
	cancel()
	if err := <-done; !IsKind(err, Interrupted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled BackupStdin() error = %v", err)
	}
	locks, err = listRealLocks(t.Context(), c, repository)
	if err != nil || strings.TrimSpace(string(locks)) != "" {
		t.Fatalf("locks after cancellation = %q, %v", locks, err)
	}
	snapshots, err := c.Snapshots(t.Context(), SnapshotsOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshots after cancellation = %+v, want none", snapshots)
	}
}

func TestRealDumpReturnsWriterError(t *testing.T) {
	t.Parallel()
	c := requireRestic(t)
	repository := newRealRepository(t, c, "dump-writer-error")
	payload := bytes.Repeat([]byte("x"), 2<<20)
	backup, err := c.BackupStdin(t.Context(), BackupStdinOptions{Repository: repository, Filename: "stack/large"}, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	writerErr := errors.New("consumer stopped")
	err = c.Dump(t.Context(), DumpOptions{Repository: repository, SnapshotID: backup.SnapshotID, Path: "/stack/large"}, &limitedErrorWriter{remaining: 1024, err: writerErr})
	if !errors.Is(err, writerErr) {
		t.Fatalf("Dump() error = %v, want writer cause", err)
	}
}

func TestRealExpectedBytesPreservesWrongPassword(t *testing.T) {
	t.Parallel()
	c := requireRestic(t)
	repository := newRealRepository(t, c, "expected-wrong-password")
	repository.Password = "wrong-expected-password"
	expected := int64(64 << 20)
	_, err := c.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: repository, Filename: "stack/member", ExpectedBytes: &expected,
	}, io.LimitReader(zeroReader{}, expected))
	if !IsKind(err, WrongPassword) {
		t.Fatalf("BackupStdin() error = %v, want WrongPassword", err)
	}
}

func TestRealCopyOfCopyUsesOriginalProvenance(t *testing.T) {
	t.Parallel()
	c := requireRestic(t)
	a := newRealRepository(t, c, "copy-chain-a")
	b := newRealRepository(t, c, "copy-chain-b")
	destination := newRealRepository(t, c, "copy-chain-c")
	first, err := c.BackupStdin(t.Context(), BackupStdinOptions{Repository: a, Filename: "stack/member"}, strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	ab, err := c.Copy(t.Context(), CopyOptions{Destination: b, Source: a, IDs: []string{first.SnapshotID}})
	if err != nil || len(ab.Snapshots) != 1 {
		t.Fatalf("A to B Copy() = %+v, %v", ab, err)
	}
	bc, err := c.Copy(t.Context(), CopyOptions{Destination: destination, Source: b, IDs: []string{ab.Snapshots[0].ID}})
	if err != nil || len(bc.Snapshots) != 1 || bc.Snapshots[0].Original != first.SnapshotID {
		t.Fatalf("B to C Copy() = %+v, %v", bc, err)
	}
}

func TestRealRepositoryMetadataAndLifecycleOperations(t *testing.T) {
	t.Parallel()
	c := requireRestic(t)
	repository := newRealRepository(t, c, "metadata-lifecycle")
	key, err := c.KeyAdd(t.Context(), KeyAddOptions{Repository: repository, NewPassword: "second-metadata-password", User: "angee", Host: "stack-a"})
	if err != nil || !fullSnapshotID.MatchString(key.ID) {
		t.Fatalf("KeyAdd() = %+v, %v", key, err)
	}
	config, err := c.CatConfig(t.Context(), repository)
	if err != nil || config.ID == "" || config.Version == 0 {
		t.Fatalf("CatConfig() = %+v, %v", config, err)
	}
	var newest BackupResult
	for index := range 3 {
		newest, err = c.BackupStdin(t.Context(), BackupStdinOptions{
			Repository: repository, Filename: "ops/file", Tags: []string{"kind=ops"}, Host: "stack-a",
		}, strings.NewReader(strconv.Itoa(index)))
		if err != nil {
			t.Fatal(err)
		}
	}
	groups, err := c.ForgetDryRunPolicy(t.Context(), ForgetDryRunOptions{
		Repository: repository,
		Filter:     SnapshotFilter{Tags: [][]string{{"kind=ops"}}, Host: "stack-a"},
		Policy:     RetentionPolicy{KeepLast: 1},
	})
	if err != nil || len(groups) == 0 || len(groups[0].Remove) != 2 {
		t.Fatalf("ForgetDryRunPolicy() = %+v, %v", groups, err)
	}
	target := t.TempDir()
	restored, err := c.Restore(t.Context(), RestoreOptions{Repository: repository, SnapshotID: newest.SnapshotID, Target: target, Includes: []string{"/ops/file"}})
	if err != nil || restored.FilesRestored == 0 {
		t.Fatalf("Restore() = %+v, %v", restored, err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "ops", "file")); err != nil || string(data) != "2" {
		t.Fatalf("restored file = %q, %v", data, err)
	}
	checked, err := c.Check(t.Context(), CheckOptions{Repository: repository, ReadDataSubset: "10%"})
	if err != nil || checked.NumErrors != 0 {
		t.Fatalf("Check() = %+v, %v", checked, err)
	}
	if _, err := c.Prune(t.Context(), repository); err != nil {
		t.Fatalf("Prune(): %v", err)
	}
}

func TestRealBackupPathsStableTree(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "paths")
	base := t.TempDir()
	member := filepath.Join(base, "databases", "app")
	if err := os.MkdirAll(member, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(member, "app.db"), []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := client.BackupPaths(t.Context(), BackupPathsOptions{
		Repository:  repository,
		BaseDir:     base,
		Paths:       []string{"databases/app"},
		Host:        "stack-a",
		IgnoreInode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := client.Ls(t.Context(), LsOptions{Repository: repository, SnapshotID: backup.SnapshotID})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listed.Nodes, func(node Node) bool { return node.Path == "/databases/app/app.db" && node.Type == "file" }) {
		t.Fatalf("snapshot nodes = %+v", listed.Nodes)
	}
}

func TestRealCopyExactIDsWithOriginal(t *testing.T) {
	t.Parallel()
	client := requireRestic(t)
	source := newRealRepository(t, client, "copy-source")
	first, err := client.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: source, Filename: "databases/app/one.dump", Tags: []string{"one"},
	}, strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.BackupStdin(t.Context(), BackupStdinOptions{
		Repository: source, Filename: "databases/app/two.dump", Tags: []string{"two"},
	}, strings.NewReader("two")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	destination := Repository{
		URL:      filepath.Join(root, "copy-destination"),
		Password: "different-destination-password",
		CacheDir: filepath.Join(root, "cache-destination"),
	}
	if _, err := client.Init(t.Context(), InitOptions{Repository: destination, CopyChunkerFrom: &source}); err != nil {
		t.Fatal(err)
	}
	copied, err := client.Copy(t.Context(), CopyOptions{
		Destination: destination,
		Source:      source,
		IDs:         []string{first.SnapshotID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.Snapshots) != 1 || copied.Snapshots[0].ID == first.SnapshotID || copied.Snapshots[0].Original != first.SnapshotID {
		t.Fatalf("Copy() = %+v", copied)
	}
	all, err := client.Snapshots(t.Context(), SnapshotsOptions{Repository: destination})
	if err != nil || len(all) != 1 {
		t.Fatalf("destination snapshots = %+v, %v", all, err)
	}
}

func TestRealBackupPathsStoresInteriorSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation differs on Windows")
	}
	t.Parallel()
	client := requireRestic(t)
	repository := newRealRepository(t, client, "symlink")
	base := t.TempDir()
	member := filepath.Join(base, "storage", "media")
	if err := os.MkdirAll(member, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(member, "target.txt"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(member, "link.txt")); err != nil {
		t.Fatal(err)
	}
	backup, err := client.BackupPaths(t.Context(), BackupPathsOptions{
		Repository: repository, BaseDir: base, Paths: []string{"storage/media"},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := client.Ls(t.Context(), LsOptions{Repository: repository, SnapshotID: backup.SnapshotID})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listed.Nodes, func(node Node) bool {
		return node.Path == "/storage/media/link.txt" && node.Type == "symlink"
	}) {
		t.Fatalf("snapshot nodes = %+v", listed.Nodes)
	}
}

type limitedErrorWriter struct {
	remaining int
	err       error
}

func (w *limitedErrorWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	if len(data) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, w.err
	}
	w.remaining -= len(data)
	return len(data), nil
}

type zeroReader struct{}

func (zeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

type stagedBlockingReader struct {
	started   chan struct{}
	release   chan struct{}
	closeErr  error
	remaining int
	startOnce sync.Once
	closeOnce sync.Once
}

func newStagedBlockingReader(closeErr error) *stagedBlockingReader {
	return &stagedBlockingReader{
		started: make(chan struct{}), release: make(chan struct{}), closeErr: closeErr,
		remaining: 256 << 10,
	}
}

func (r *stagedBlockingReader) Read(data []byte) (int, error) {
	if r.remaining > 0 {
		n := min(len(data), r.remaining)
		clear(data[:n])
		r.remaining -= n
		return n, nil
	}
	r.startOnce.Do(func() { close(r.started) })
	<-r.release
	return 0, r.closeErr
}

func (r *stagedBlockingReader) Close() error {
	r.closeOnce.Do(func() { close(r.release) })
	return nil
}

func listRealLocks(ctx context.Context, c *Client, repository Repository) ([]byte, error) {
	result, err := c.run(ctx, invocation{repository: &repository, args: []string{"list", "locks"}, metadata: true, noLock: true})
	return result.Stdout, err
}
