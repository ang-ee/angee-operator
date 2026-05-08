package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestOpenCommandsVSCodeWithCodeWorkspace(t *testing.T) {
	cmds, err := openCommands(editorVSCode, "/ws", "/ws/foo.code-workspace", []string{"a", "b"}, "darwin")
	if err != nil {
		t.Fatalf("openCommands: %v", err)
	}
	want := [][]string{{"code", "/ws/foo.code-workspace"}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v, want %v", cmds, want)
	}
}

func TestOpenCommandsVSCodeFallbackToDir(t *testing.T) {
	cmds, err := openCommands(editorVSCode, "/ws", "", []string{"a"}, "linux")
	if err != nil {
		t.Fatalf("openCommands: %v", err)
	}
	want := [][]string{{"code", "/ws"}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v, want %v", cmds, want)
	}
}

func TestOpenCommandsIDEA(t *testing.T) {
	cmds, err := openCommands(editorIDEA, "/ws", "/ws/foo.code-workspace", []string{"a"}, "linux")
	if err != nil {
		t.Fatalf("openCommands: %v", err)
	}
	want := [][]string{{"idea", "/ws"}}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v, want %v", cmds, want)
	}
}

func TestOpenCommandsGHDesktopMacOS(t *testing.T) {
	cmds, err := openCommands(editorGHDesktop, "/ws", "", []string{"django-angee", "angee-go", "angee-examples"}, "darwin")
	if err != nil {
		t.Fatalf("openCommands: %v", err)
	}
	want := [][]string{
		{"open", "-a", "GitHub Desktop", "/ws/django-angee"},
		{"open", "-a", "GitHub Desktop", "/ws/angee-go"},
		{"open", "-a", "GitHub Desktop", "/ws/angee-examples"},
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("cmds = %v, want %v", cmds, want)
	}
}

func TestOpenCommandsGHDesktopRefusesNonDarwin(t *testing.T) {
	_, err := openCommands(editorGHDesktop, "/ws", "", []string{"a"}, "linux")
	if err == nil {
		t.Fatal("expected error on linux, got nil")
	}
	if !strings.Contains(err.Error(), "macOS-only") {
		t.Fatalf("error %q does not mention macOS-only constraint", err)
	}
}

func TestOpenCommandsGHDesktopRefusesEmptyRepos(t *testing.T) {
	_, err := openCommands(editorGHDesktop, "/ws", "", nil, "darwin")
	if err == nil {
		t.Fatal("expected error on empty repos, got nil")
	}
	if !strings.Contains(err.Error(), "no materialised worktrees") {
		t.Fatalf("error %q does not mention missing worktrees", err)
	}
}

func TestOpenCommandsUnknownEditor(t *testing.T) {
	_, err := openCommands(editorChoice("emacs"), "/ws", "", nil, "darwin")
	if err == nil {
		t.Fatal("expected error on unknown editor, got nil")
	}
}

func TestMaterialisedWorktrees(t *testing.T) {
	root := t.TempDir()

	// repo with .git directory (mimics a primary checkout)
	mustMkdir(t, filepath.Join(root, "repo-a"))
	mustMkdir(t, filepath.Join(root, "repo-a", ".git"))

	// repo with .git file (mimics a worktree — git stores `gitdir: …` in a file)
	mustMkdir(t, filepath.Join(root, "repo-b"))
	mustWriteFile(t, filepath.Join(root, "repo-b", ".git"), "gitdir: /elsewhere\n")

	// not-a-repo directory
	mustMkdir(t, filepath.Join(root, "not-a-repo"))

	// stray file at top level — must not be treated as a repo
	mustWriteFile(t, filepath.Join(root, "README.md"), "hi")

	got, err := materialisedWorktrees(root)
	if err != nil {
		t.Fatalf("materialisedWorktrees: %v", err)
	}
	want := []string{"repo-a", "repo-b"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repos = %v, want %v", got, want)
	}
}

func TestMaterialisedWorktreesSkipsBrokenSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()

	// Dangling symlink — target does not exist. Must be silently skipped.
	if err := os.Symlink(filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(root, "broken")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// One real repo so we have something to assert against.
	mustMkdir(t, filepath.Join(root, "real"))
	mustMkdir(t, filepath.Join(root, "real", ".git"))

	got, err := materialisedWorktrees(root)
	if err != nil {
		t.Fatalf("materialisedWorktrees: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"real"}) {
		t.Fatalf("repos = %v, want [real] (broken symlink should be skipped)", got)
	}
}

func TestValidateEditorChoice(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"vscode", false},
		{"idea", false},
		{"gh-desktop", false},
		{"emacs", true},
		{"", true},
		{"VSCode", true}, // case-sensitive on purpose
	}
	for _, tc := range cases {
		err := validateEditorChoice(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateEditorChoice(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

func TestEnsureWorkspaceOpenLocalRejectsRemoteOperator(t *testing.T) {
	operatorURL := "http://127.0.0.1:8080"
	err := ensureWorkspaceOpenLocal(&operatorURL)
	if err == nil {
		t.Fatal("expected remote operator mode error, got nil")
	}
	if !strings.Contains(err.Error(), "--operator/ANGEE_OPERATOR_URL") {
		t.Fatalf("error %q does not mention remote operator configuration", err)
	}
}

func TestLaunchOpenCommandsFailsWhenNoBinaryLaunched(t *testing.T) {
	var stderr bytes.Buffer
	startCalled := false
	err := launchOpenCommands(
		[][]string{{"missing-editor", "/ws/demo"}},
		&stderr,
		func(string) (string, error) { return "", errors.New("not found") },
		func([]string) error {
			startCalled = true
			return nil
		},
	)
	if err == nil {
		t.Fatal("expected error when all editor binaries are missing, got nil")
	}
	if startCalled {
		t.Fatal("start should not be called for missing binaries")
	}
	if !strings.Contains(err.Error(), "no editor command launched") {
		t.Fatalf("error %q does not mention missing launch", err)
	}
	if got := stderr.String(); !strings.Contains(got, `binary "missing-editor" not found`) {
		t.Fatalf("stderr = %q, want missing binary message", got)
	}
}

func TestLaunchOpenCommandsSucceedsWhenAtLeastOneCommandLaunches(t *testing.T) {
	var stderr bytes.Buffer
	var launched []string
	err := launchOpenCommands(
		[][]string{
			{"missing-editor", "/ws/demo"},
			{"real-editor", "/ws/demo"},
		},
		&stderr,
		func(name string) (string, error) {
			if name == "missing-editor" {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		func(c []string) error {
			launched = append(launched, c[0])
			return nil
		},
	)
	if err != nil {
		t.Fatalf("launchOpenCommands: %v", err)
	}
	if !reflect.DeepEqual(launched, []string{"real-editor"}) {
		t.Fatalf("launched = %v, want [real-editor]", launched)
	}
	if got := stderr.String(); !strings.Contains(got, `binary "missing-editor" not found`) {
		t.Fatalf("stderr = %q, want missing binary message", got)
	}
}

func TestMaterialisedWorktreesFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()

	target := filepath.Join(t.TempDir(), "real-repo")
	mustMkdir(t, target)
	mustMkdir(t, filepath.Join(target, ".git"))

	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := materialisedWorktrees(root)
	if err != nil {
		t.Fatalf("materialisedWorktrees: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"linked"}) {
		t.Fatalf("repos = %v, want [linked]", got)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWriteFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
