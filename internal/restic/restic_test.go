package restic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

type stubRunner struct {
	commands []Command
	run      func(Command) (CommandResult, error)
}

func (runner *stubRunner) Run(_ context.Context, command Command) (CommandResult, error) {
	clone := command
	clone.Args = append([]string(nil), command.Args...)
	clone.Env = append([]string(nil), command.Env...)
	clone.Passwords = append([]string(nil), command.Passwords...)
	clone.Secrets = append([]string(nil), command.Secrets...)
	runner.commands = append(runner.commands, clone)
	if runner.run != nil {
		return runner.run(command)
	}
	return CommandResult{}, nil
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testRepository(t *testing.T) Repository {
	t.Helper()
	return Repository{
		URL:      "s3:https://repo-user:repo-secret@example.invalid/bucket?token=query-secret",
		Password: "distinctive-repository-password",
		Env:      []string{"AWS_ACCESS_KEY_ID=backend-access", "AWS_SECRET_ACCESS_KEY=backend-secret"},
		CacheDir: t.TempDir(),
	}
}

func TestFixtureDecodingToleratesUnknownFields(t *testing.T) {
	var version VersionInfo
	if err := decodeJSON(fixture(t, "version.json"), &version); err != nil || version.Version != MinVersion {
		t.Fatalf("version = %+v, %v", version, err)
	}
	summary, err := backupSummary(fixture(t, "backup.ndjson"))
	if err != nil || summary.SnapshotID == "" || summary.TotalBytesProcessed != 7 {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	var snapshots []Snapshot
	if err := decodeJSON(fixture(t, "snapshots.json"), &snapshots); err != nil || len(snapshots) != 1 || snapshots[0].Hostname != "stackA" {
		t.Fatalf("snapshots = %+v, %v", snapshots, err)
	}
	var groups []ForgetGroup
	if err := decodeJSON(fixture(t, "forget.json"), &groups); err != nil || len(groups) != 1 || len(groups[0].Keep) != 1 {
		t.Fatalf("groups = %+v, %v", groups, err)
	}
	var check CheckSummary
	if err := decodeJSON(fixture(t, "check.json"), &check); err != nil || check.NumErrors != 0 {
		t.Fatalf("check = %+v, %v", check, err)
	}
}

func TestLsDecodesFixture(t *testing.T) {
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: fixture(t, "ls.ndjson")}, nil
	}}
	c := &Client{Runner: runner, Binary: "restic-test"}
	listed, err := c.Ls(t.Context(), LsOptions{Repository: testRepository(t), SnapshotID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Snapshot.ID == "" || len(listed.Nodes) != 2 || listed.Nodes[1].Path != "/databases/app/app.pgdump" {
		t.Fatalf("Ls() = %+v", listed)
	}
}

func TestExitErrorParsingIgnoresPlainText(t *testing.T) {
	code, message := exitErrorMessage(fixture(t, "exit-error.stderr"))
	if code != 3 || message != "Warning: at least one source file could not be read" {
		t.Fatalf("exitErrorMessage() = %d, %q", code, message)
	}
}

func TestExitCodesMapToKinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper invokes a POSIX shell")
	}
	for code, want := range map[int]Kind{10: NotFound, 11: Locked, 12: WrongPassword, 3: WriteFailure, 130: Interrupted, 1: Failure} {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		got := commandError(t.Context(), err, []byte(`{"message_type":"exit_error","code":`+strconv.Itoa(code)+`,"message":"restic message"}`), nil)
		if got.Kind != want || got.ExitCode != code || got.Message != "restic message" {
			t.Errorf("code %d => %+v, want %s", code, got, want)
		}
	}
	err := exec.Command("sh", "-c", "exit 1").Run()
	missing := commandError(t.Context(), err, []byte(`{"message_type":"exit_error","code":1,"message":"Fatal: failed to find snapshot: no matching ID found"}`), nil)
	if missing.Kind != NotFound {
		t.Fatalf("unknown snapshot => %s, want %s", missing.Kind, NotFound)
	}
	redactedMissing := commandError(t.Context(), err, []byte(`{"message_type":"exit_error","code":1,"message":"Fatal: failed to find snapshot: no matching ID found"}`), []string{"find"})
	if redactedMissing.Kind != NotFound || strings.Contains(redactedMissing.Message, "find") {
		t.Fatalf("redacted unknown snapshot => %+v", redactedMissing)
	}
	jsonZero := commandError(t.Context(), exec.Command("sh", "-c", "exit 12").Run(), []byte(`{"message_type":"exit_error","message":"wrong password"}`), nil)
	if jsonZero.Kind != WrongPassword || jsonZero.ExitCode != 12 {
		t.Fatalf("process exit code preference => %+v", jsonZero)
	}
}

func TestVersionFloor(t *testing.T) {
	if !VersionAtLeast("restic 0.19.1 compiled with go1.26", MinVersion) || !VersionAtLeast("0.20.0", MinVersion) || VersionAtLeast("0.19.0", MinVersion) {
		t.Fatal("VersionAtLeast returned an incorrect comparison")
	}
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{"message_type":"version","version":"0.18.0"}`)}, nil
	}}
	_, err := (&Client{Runner: runner, Binary: "restic-test"}).Version(t.Context())
	if err == nil || !IsKind(err, Unavailable) || !strings.Contains(err.Error(), "older than required 0.19.1") || strings.Count(err.Error(), InstallPackage) != 1 {
		t.Fatalf("Version() error = %v", err)
	}
}

func TestRepositoryInvocationStripsInheritedResticEnvironment(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "parent-password")
	t.Setenv("RESTIC_PASSWORD_COMMAND", "parent-command")
	t.Setenv("RESTIC_REPOSITORY_FILE", "/parent/repository-file")
	t.Setenv("RESTIC_REPOSITORY", "/parent/repository")
	t.Setenv("RESTIC_HOST", "parent-host")
	t.Setenv("RESTIC_FROM_PASSWORD_FILE", "/parent/from-password")
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-aws")
	t.Setenv("GH_TOKEN", "ambient-gh")
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte("[]")}, nil
	}}
	repository := testRepository(t)
	client := &Client{Runner: runner, Binary: "restic-test"}
	_, err := client.Snapshots(t.Context(), SnapshotsOptions{Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	command := runner.commands[0]
	if slices.Contains(command.Args, repository.URL) || slices.Contains(command.Args, repository.Password) {
		t.Fatalf("argv leaked repository data: %#v", command.Args)
	}
	if !reflect.DeepEqual(command.Passwords, []string{repository.Password}) {
		t.Fatalf("passwords = %#v", command.Passwords)
	}
	env := strings.Join(command.Env, "\n")
	for _, absent := range []string{"parent-password", "parent-command", "/parent/repository-file", "/parent/repository", "parent-host", "/parent/from-password", "ambient-aws", "ambient-gh", "RESTIC_PASSWORD=", "RESTIC_PASSWORD_COMMAND=", "RESTIC_REPOSITORY_FILE=", "RESTIC_HOST=", "RESTIC_FROM_PASSWORD_FILE="} {
		if strings.Contains(env, absent) {
			t.Fatalf("child env retained %q:\n%s", absent, env)
		}
	}
	for _, present := range []string{
		"RESTIC_REPOSITORY=" + repository.URL,
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
		"RESTIC_CACHE_DIR=" + repository.CacheDir,
		"AWS_SECRET_ACCESS_KEY=backend-secret",
	} {
		if !strings.Contains(env, present) {
			t.Fatalf("child env missing %q:\n%s", present, env)
		}
	}
	versionRunner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: fixture(t, "version.json")}, nil
	}}
	if _, err := (&Client{Runner: versionRunner, Binary: "restic-test"}).Version(t.Context()); err != nil {
		t.Fatal(err)
	}
	versionEnv := strings.Join(versionRunner.commands[0].Env, "\n")
	for _, absent := range []string{"ambient-aws", "ambient-gh", "RESTIC_REPOSITORY=", "RESTIC_HOST=", "RESTIC_FROM_PASSWORD_FILE="} {
		if strings.Contains(versionEnv, absent) {
			t.Fatalf("version child env retained %q:\n%s", absent, versionEnv)
		}
	}
}

func TestCopyUsesTwoPasswordFDsAndExactIDs(t *testing.T) {
	const sourceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	destination := testRepository(t)
	source := testRepository(t)
	source.URL = "/source/repository"
	source.Password = "distinctive-source-password"
	call := 0
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		call++
		if call == 1 {
			return CommandResult{Stdout: []byte(`[{"id":"` + sourceID + `"}]`)}, nil
		}
		if call == 2 {
			return CommandResult{Stdout: []byte("copy complete\n")}, nil
		}
		return CommandResult{Stdout: []byte(`[{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","original":"` + sourceID + `"}]`)}, nil
	}}
	result, err := (&Client{Runner: runner, Binary: "restic-test"}).Copy(t.Context(), CopyOptions{
		Destination: destination,
		Source:      source,
		IDs:         []string{sourceID},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := runner.commands[1]
	if !reflect.DeepEqual(command.Passwords, []string{destination.Password, source.Password}) {
		t.Fatalf("passwords = %#v", command.Passwords)
	}
	if !containsSequence(command.Args, "--from-password-file", "/dev/fd/4") || !slices.Contains(command.Args, sourceID) {
		t.Fatalf("copy args = %#v", command.Args)
	}
	if len(result.Snapshots) != 1 || result.Snapshots[0].Original != sourceID {
		t.Fatalf("Copy() = %+v", result)
	}
}

func TestCopyMissingSourceUsesAdapterExitCode(t *testing.T) {
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte("[]")}, nil
	}}
	_, err := (&Client{Runner: runner, Binary: "restic-test"}).Copy(t.Context(), CopyOptions{
		Destination: testRepository(t),
		Source:      testRepository(t),
		IDs:         []string{strings.Repeat("a", 64)},
	})
	var resticErr *Error
	if !errors.As(err, &resticErr) || resticErr.Kind != NotFound || resticErr.ExitCode != -1 {
		t.Fatalf("Copy() error = %#v, want adapter NotFound with exit code -1", err)
	}
}

func TestRepositoryEnvironmentRejectsConflictingSourceValue(t *testing.T) {
	destination := testRepository(t)
	source := testRepository(t)
	destination.Env = []string{"AWS_ACCESS_KEY_ID=destination"}
	source.Env = []string{"AWS_ACCESS_KEY_ID=source"}
	_, _, _, err := invocationEnvironment(invocation{repository: &destination, source: &source})
	if err == nil || !strings.Contains(err.Error(), "conflicting backend environment") {
		t.Fatalf("Copy() error = %v", err)
	}
}

func TestSnapshotFilterBuildsANDORArguments(t *testing.T) {
	runner := &stubRunner{run: func(Command) (CommandResult, error) { return CommandResult{Stdout: []byte("[]")}, nil }}
	_, err := (&Client{Runner: runner, Binary: "restic-test"}).Snapshots(t.Context(), SnapshotsOptions{
		Repository: testRepository(t),
		Filter: SnapshotFilter{
			Tags:  [][]string{{"a", "b"}, {"c"}},
			Host:  "stack-a",
			Paths: []string{"/one", "/two"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := runner.commands[0].Args
	if !slices.Contains(args, "--no-lock") || slices.Contains(args, "--retry-lock") {
		t.Fatalf("read-only snapshot args = %#v", args)
	}
	for _, sequence := range [][2]string{{"--tag", "a,b"}, {"--tag", "c"}, {"--host", "stack-a"}, {"--path", "/one"}, {"--path", "/two"}} {
		if !containsSequence(args, sequence[0], sequence[1]) {
			t.Fatalf("args = %#v, missing %#v", args, sequence)
		}
	}
}

func TestSnapshotMetadataRejectsResticRewrites(t *testing.T) {
	for _, tags := range [][]string{{""}, {"a,b"}, {" spaced"}, {"spaced "}} {
		_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).Snapshots(t.Context(), SnapshotsOptions{
			Repository: testRepository(t), Filter: SnapshotFilter{Tags: [][]string{tags}},
		})
		if err == nil {
			t.Errorf("Snapshots accepted tags %q", tags)
		}
	}
	for _, host := range []string{" host", "host ", "host,other"} {
		_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).BackupStdin(t.Context(), BackupStdinOptions{
			Repository: testRepository(t), Filename: "stack/member", Host: host,
		}, strings.NewReader("data"))
		if err == nil {
			t.Errorf("BackupStdin accepted host %q", host)
		}
	}
	_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).Snapshots(t.Context(), SnapshotsOptions{
		Repository: testRepository(t), Filter: SnapshotFilter{Tags: [][]string{{}}},
	})
	if err == nil {
		t.Fatal("Snapshots accepted an empty tag group")
	}
}

func TestBackupStdinValidationAndReaderError(t *testing.T) {
	for _, name := range []string{"", "/absolute", "../escape", "a/../b", "a//b", "a/./b", "trailing/", `a\b`} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).BackupStdin(t.Context(), BackupStdinOptions{
				Repository: testRepository(t), Filename: name,
			}, strings.NewReader("data"))
			if err == nil {
				t.Fatalf("BackupStdin accepted %q", name)
			}
		})
	}

	readerErr := errors.New("producer stopped early")
	runner := &stubRunner{run: func(command Command) (CommandResult, error) {
		_, err := io.ReadAll(command.Stdin)
		return CommandResult{Stdout: fixture(t, "backup.ndjson"), StdinError: err, StdinAborted: true}, nil
	}}
	result, err := (&Client{Runner: runner, Binary: "restic-test"}).BackupStdin(t.Context(), BackupStdinOptions{
		Repository: testRepository(t), Filename: "databases/app/app.pgdump",
	}, &dataErrorReader{data: []byte("partial"), err: readerErr})
	if result.SnapshotID == "" || err == nil || !errors.Is(err, readerErr) || !strings.Contains(err.Error(), "committed snapshot") {
		t.Fatalf("BackupStdin() = %+v, %v", result, err)
	}
}

func TestStreamErrorsUseInvocationLocalRedaction(t *testing.T) {
	repository := testRepository(t)
	repository.URL = "s3:https://zorro-local:url-secret@example.invalid/bucket?token=q3"
	repository.Password = "p1"
	repository.Env = []string{"BACKEND_TOKEN=b2"}
	secretText := repository.URL + " username=zorro-local password=p1 backend=b2"

	readerErr := errors.New("producer exposed " + secretText)
	backupRunner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: fixture(t, "backup.ndjson"), StdinError: readerErr, StdinAborted: true}, nil
	}}
	_, err := (&Client{Runner: backupRunner, Binary: "restic-test"}).BackupStdin(t.Context(), BackupStdinOptions{
		Repository: repository, Filename: "stack/member",
	}, strings.NewReader("data"))
	assertErrorRedacted(t, err, readerErr, repository.URL, "zorro-local", "p1", "b2")

	writerErr := errors.New("consumer exposed " + secretText)
	dumpRunner := &stubRunner{run: func(command Command) (CommandResult, error) {
		_, writeErr := command.Stdout.Write([]byte("data"))
		return CommandResult{}, writeErr
	}}
	err = (&Client{Runner: dumpRunner, Binary: "restic-test"}).Dump(t.Context(), DumpOptions{
		Repository: repository, SnapshotID: strings.Repeat("a", 64), Path: "/stack/member",
	}, errorWriter{err: writerErr})
	assertErrorRedacted(t, err, writerErr, repository.URL, "zorro-local", "p1", "b2")
}

func assertErrorRedacted(t *testing.T, err error, cause error, secrets ...string) {
	t.Helper()
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want cause %v", err, cause)
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked %q: %v", secret, err)
		}
	}
}

func TestBackupPathsRejectsUnsafeMembers(t *testing.T) {
	for _, path := range []string{"", ".", "/absolute", "../escape", "a/../b", "a//b", "a/./b"} {
		_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).BackupPaths(t.Context(), BackupPathsOptions{
			Repository: testRepository(t), BaseDir: t.TempDir(), Paths: []string{path},
		})
		if err == nil {
			t.Errorf("BackupPaths accepted %q", path)
		}
	}
	_, err := (&Client{Binary: "unused", Runner: &stubRunner{}}).BackupPaths(t.Context(), BackupPathsOptions{
		Repository: testRepository(t), BaseDir: "relative", Paths: []string{"safe"},
	})
	if err == nil {
		t.Fatal("BackupPaths accepted a relative BaseDir")
	}
}

func TestBackupPathsPassesIgnoreInode(t *testing.T) {
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: fixture(t, "backup.ndjson")}, nil
	}}
	_, err := (&Client{Binary: "restic-test", Runner: runner}).BackupPaths(t.Context(), BackupPathsOptions{
		Repository: testRepository(t), BaseDir: t.TempDir(), Paths: []string{"member"}, IgnoreInode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(runner.commands[0].Args, "--ignore-inode") {
		t.Fatalf("BackupPaths args = %#v", runner.commands[0].Args)
	}
}

func TestSnapshotOperationsRequireFullIDs(t *testing.T) {
	c := &Client{Binary: "unused", Runner: &stubRunner{}}
	repository := testRepository(t)
	if _, err := c.Ls(t.Context(), LsOptions{Repository: repository, SnapshotID: "latest"}); err == nil {
		t.Fatal("Ls accepted latest")
	}
	if err := c.Dump(t.Context(), DumpOptions{Repository: repository, SnapshotID: "deadbeef", Path: "/x"}, io.Discard); err == nil {
		t.Fatal("Dump accepted a prefix")
	}
	if _, err := c.Restore(t.Context(), RestoreOptions{Repository: repository, SnapshotID: "latest", Target: t.TempDir()}); err == nil {
		t.Fatal("Restore accepted latest")
	}
	if _, err := c.Forget(t.Context(), ForgetOptions{Repository: repository, IDs: []string{"deadbeef"}}); err == nil {
		t.Fatal("Forget accepted a prefix")
	}
	if _, err := c.BackupPaths(t.Context(), BackupPathsOptions{
		Repository: repository, BaseDir: t.TempDir(), Paths: []string{"stack"}, Parent: "deadbeef",
	}); err == nil {
		t.Fatal("BackupPaths accepted a parent prefix")
	}
}

func TestExecRunnerWiresFD3AndFD4(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inherited Unix file descriptors")
	}
	result, err := (ExecRunner{}).Run(t.Context(), Command{
		Name: "/bin/sh",
		Args: []string{"-c", `IFS= read -r first <&3; IFS= read -r second <&4; printf '%s|%s' "$first" "$second"`},
		Env:  []string{"PATH=/usr/bin:/bin"},
		Passwords: []string{
			"destination-password",
			"source-password",
		},
	})
	if err != nil || string(result.Stdout) != "destination-password|source-password" {
		t.Fatalf("Run() = %q, %q, %v", result.Stdout, result.Stderr, err)
	}
}

func TestFailureRedactsSecretsFromErrorAndTrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a shell script and inherited fd")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "restic-helper")
	script := `#!/bin/sh
IFS= read -r password <&3
printf 'plain repository=%s backend=%s password=%s\n' "$RESTIC_REPOSITORY" "$BACKEND_TOKEN" "$password" >&2
printf 'isolated token=%s\n' "${RESTIC_REPOSITORY##*token=}" >&2
printf '{"message_type":"exit_error","code":1,"message":"failed %s %s %s"}\n' "$RESTIC_REPOSITORY" "$BACKEND_TOKEN" "$password" >&2
exit 1
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	repository := testRepository(t)
	repository.URL = "s3:https://zorro-round1:p@example.invalid/bucket?token=q!3"
	repository.Password = "p@1"
	repository.Env = []string{"BACKEND_TOKEN=b#2"}
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	_, err := (&Client{Runner: ExecRunner{}, Binary: helper}).Snapshots(ctx, SnapshotsOptions{Repository: repository})
	if err == nil {
		t.Fatal("Snapshots() error is nil")
	}
	all := err.Error() + "\n" + logs.String()
	for _, secret := range []string{repository.URL, "zorro-round1", "q!3", repository.Password, "b#2"} {
		if strings.Contains(all, secret) {
			t.Fatalf("diagnostic leaked %q:\n%s", secret, all)
		}
	}
	var resticErr *Error
	if !errors.As(err, &resticErr) || strings.Contains(resticErr.Hint, "b#2") {
		t.Fatalf("error = %#v", err)
	}
	if got := logctx.RedactText("zorro-round1 rides at dawn"); got != "zorro-round1 rides at dawn" {
		t.Fatalf("repository username was registered process-wide: %q", got)
	}
}

func TestTextResultUsesInvocationLocalRedaction(t *testing.T) {
	repository := testRepository(t)
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte("forgot " + repository.URL + " using backend-secret")}, nil
	}}
	result, err := (&Client{Runner: runner, Binary: "restic-test"}).Forget(t.Context(), ForgetOptions{
		Repository: repository, IDs: []string{strings.Repeat("a", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Output, repository.URL) || strings.Contains(result.Output, "backend-secret") {
		t.Fatalf("Forget output leaked secrets: %q", result.Output)
	}
}

func TestBackupStdinCancellationBoundsBlockingReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a shell script and inherited fd")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "restic-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexec /bin/cat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	readerClosed := errors.New("blocking reader closed")
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), closeErr: readerClosed}
	repository := testRepository(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := (&Client{Runner: ExecRunner{}, Binary: helper}).BackupStdin(ctx, BackupStdinOptions{
			Repository: repository,
			Filename:   "databases/app/app.pgdump",
		}, reader)
		done <- err
	}()
	<-reader.started
	cancel()

	select {
	case err := <-done:
		if !IsKind(err, Interrupted) || !errors.Is(err, context.Canceled) {
			t.Fatalf("BackupStdin() error = %v", err)
		}
		if got := reader.closeCalls.Load(); got != 1 {
			t.Fatalf("reader Close calls = %d, want 1", got)
		}
	case <-time.After(3 * time.Second):
		_ = reader.Close()
		err := <-done
		t.Fatalf("BackupStdin() did not honor cancellation: %v", err)
	}
}

func TestBackupStdinEarlyExitClosesBlockedReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a shell script and inherited fd")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "restic-helper")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"message_type\":\"exit_error\",\"code\":12,\"message\":\"wrong password\"}' >&2\nexit 12\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), closeErr: errors.New("closed")}
	_, err := (&Client{Runner: ExecRunner{}, Binary: helper}).BackupStdin(t.Context(), BackupStdinOptions{
		Repository: testRepository(t), Filename: "stack/member",
	}, reader)
	if !IsKind(err, WrongPassword) {
		t.Fatalf("BackupStdin() error = %v, want WrongPassword", err)
	}
}

func TestBackupStdinEarlyExitBoundsNopCloserReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper uses a shell script and inherited fd")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "restic-helper")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"message_type\":\"exit_error\",\"code\":12,\"message\":\"wrong password\"}' >&2\nexit 12\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	underlying := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), closeErr: errors.New("released")}
	t.Cleanup(func() { _ = underlying.Close() })
	repository := testRepository(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Client{Runner: ExecRunner{}, Binary: helper}).BackupStdin(ctx, BackupStdinOptions{
			Repository: repository, Filename: "stack/member",
		}, io.NopCloser(underlying))
		done <- err
	}()
	select {
	case err := <-done:
		if !IsKind(err, WrongPassword) {
			t.Fatalf("BackupStdin() error = %v, want WrongPassword", err)
		}
	case <-time.After(2 * time.Second):
		_ = underlying.Close()
		err := <-done
		t.Fatalf("BackupStdin() did not bound a NopCloser-wrapped blocked reader: %v", err)
	}
}

func TestCheckReturnsSummaryWithRunError(t *testing.T) {
	runErr := errors.New("check failed")
	runner := &stubRunner{run: func(Command) (CommandResult, error) {
		return CommandResult{Stdout: []byte(`{"message_type":"summary","num_errors":2,"broken_packs":["pack"]}`)}, runErr
	}}
	summary, err := (&Client{Runner: runner, Binary: "restic-test"}).Check(t.Context(), CheckOptions{Repository: testRepository(t)})
	if !errors.Is(err, runErr) || summary.NumErrors != 2 || !slices.Equal(summary.BrokenPacks, []string{"pack"}) {
		t.Fatalf("Check() = %+v, %v", summary, err)
	}
}

func TestUnlockTakesNoLock(t *testing.T) {
	runner := &stubRunner{}
	if _, err := (&Client{Runner: runner, Binary: "restic-test"}).Unlock(t.Context(), testRepository(t)); err != nil {
		t.Fatal(err)
	}
	args := runner.commands[0].Args
	if slices.Contains(args, "--retry-lock") {
		t.Fatalf("Unlock args = %#v, want no retry-lock", args)
	}
}

func TestCommandErrorPreservesAdapterKind(t *testing.T) {
	want := adapterError(Unavailable, -1, "unavailable", "use a supported host", nil)
	if got := commandError(t.Context(), want, nil, nil); got != want {
		t.Fatalf("commandError() = %#v, want original %#v", got, want)
	}
}

func TestResolveBinaryCachesLookup(t *testing.T) {
	lookups := 0
	c := &Client{lookupPath: func(string) (string, error) {
		lookups++
		return "/tmp/restic", nil
	}}
	for range 2 {
		if path, err := c.ResolveBinary(t.Context()); err != nil || path != "/tmp/restic" {
			t.Fatalf("ResolveBinary() = %q, %v", path, err)
		}
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1", lookups)
	}
}

func TestResolveBinaryRetriesAfterCanceledLookup(t *testing.T) {
	dir := t.TempDir()
	name := "restic"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	c := &Client{
		lookupPath: func(string) (string, error) {
			return "", exec.ErrNotFound
		},
		goBinPath: func(ctx context.Context) (string, error) {
			lookups++
			if lookups == 1 {
				return "", ctx.Err()
			}
			return dir, nil
		},
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if path, err := c.ResolveBinary(canceled); err == nil || path != "" {
		t.Fatalf("first ResolveBinary() = %q, %v, want an error", path, err)
	}
	if resolved, err := c.ResolveBinary(t.Context()); err != nil || resolved != path {
		t.Fatalf("second ResolveBinary() = %q, %v, want %q", resolved, err, path)
	}
	if lookups != 2 {
		t.Fatalf("go bin lookups = %d, want 2", lookups)
	}
}

func TestResolveBinaryConcurrentWaitHonorsContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c := &Client{lookupPath: func(string) (string, error) {
		close(started)
		<-release
		return "/tmp/restic", nil
	}}
	first := make(chan error, 1)
	go func() {
		_, err := c.ResolveBinary(t.Context())
		first <- err
	}()
	<-started
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if path, err := c.ResolveBinary(canceled); !errors.Is(err, context.Canceled) || path != "" {
		t.Fatalf("concurrent ResolveBinary() = %q, %v, want context cancellation", path, err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first ResolveBinary() error = %v", err)
	}
}

func TestRepositoryPasswordsRejectSurroundingWhitespace(t *testing.T) {
	for _, password := range []string{" leading", "trailing ", "line\n"} {
		repository := testRepository(t)
		repository.Password = password
		_, err := (&Client{Runner: &stubRunner{}, Binary: "unused"}).Snapshots(t.Context(), SnapshotsOptions{Repository: repository})
		if err == nil {
			t.Errorf("Snapshots accepted password %q", password)
		}
	}
	if _, err := (&Client{Runner: &stubRunner{}, Binary: "unused"}).KeyAdd(t.Context(), KeyAddOptions{
		Repository: testRepository(t), NewPassword: " trailing ",
	}); err == nil {
		t.Fatal("KeyAdd accepted a password with surrounding whitespace")
	}
}

func TestRepositorySecretValuesRegisterOnlyCredentialQueryValues(t *testing.T) {
	repository := Repository{
		URL:      "rest:https://user:url-password@example.invalid/repo?ref=round2-main-ref&access_token=query-token",
		Password: "repository-password",
		CacheDir: t.TempDir(),
	}
	perCall, processWide := repositorySecretValues(repository)
	for _, value := range []string{"round2-main-ref", "query-token", "url-password"} {
		if !slices.Contains(perCall, value) {
			t.Errorf("per-call values = %q, missing %q", perCall, value)
		}
	}
	if slices.Contains(processWide, "round2-main-ref") {
		t.Fatalf("process-wide values include non-credential query value: %q", processWide)
	}
	for _, value := range []string{"repository-password", "query-token", "url-password"} {
		if !slices.Contains(processWide, value) {
			t.Errorf("process-wide values = %q, missing %q", processWide, value)
		}
	}
	if _, _, _, err := invocationEnvironment(invocation{repository: &repository}); err != nil {
		t.Fatal(err)
	}
	if got := logctx.RedactText("branch=round2-main-ref"); got != "branch=round2-main-ref" {
		t.Fatalf("non-credential query value was registered process-wide: %q", got)
	}
}

func containsSequence(values []string, first string, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

type dataErrorReader struct {
	data []byte
	err  error
	done bool
}

type blockingReader struct {
	started    chan struct{}
	release    chan struct{}
	startOnce  sync.Once
	closeOnce  sync.Once
	closeErr   error
	closeCalls atomic.Int32
}

func (reader *blockingReader) Read([]byte) (int, error) {
	reader.startOnce.Do(func() { close(reader.started) })
	<-reader.release
	return 0, reader.closeErr
}

func (reader *blockingReader) Close() error {
	reader.closeCalls.Add(1)
	reader.closeOnce.Do(func() { close(reader.release) })
	return nil
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

func (reader *dataErrorReader) Read(buffer []byte) (int, error) {
	if reader.done {
		return 0, reader.err
	}
	reader.done = true
	return copy(buffer, reader.data), reader.err
}
