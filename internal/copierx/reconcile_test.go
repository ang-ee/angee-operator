package copierx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func writeReconcileTemplate(t *testing.T, root, body string) string {
	t.Helper()
	template := filepath.Join(root, "template-source")
	if err := os.MkdirAll(filepath.Join(template, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template): %v", err)
	}
	config := "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n"
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "managed.txt.jinja"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(managed.txt.jinja): %v", err)
	}
	return template
}

func openTestTrustedRoot(t *testing.T, path string) *TrustedRoot {
	t.Helper()
	root, err := OpenTrustedRoot(path)
	if err != nil {
		t.Fatalf("OpenTrustedRoot(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

type fileInfoWithMode struct {
	fs.FileInfo
	mode fs.FileMode
}

func (i fileInfoWithMode) Mode() fs.FileMode {
	return i.mode
}

func TestSameGuardedEntryIdentityRejectsFileTypeChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	replacement := fileInfoWithMode{FileInfo: original, mode: original.Mode() | os.ModeSymlink}
	if sameGuardedEntryIdentity(original, replacement) {
		t.Fatal("entry identity accepted a replacement with the same filesystem identity but a different file type")
	}
}

func TestGuardedPathWriteFileRejectsReusedInodeFileTypeSwapBeforeMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "entry")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(original): %v", err)
	}
	guard, err := OpenGuardedPath(root, root, "entry", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath: %v", err)
	}
	defer guard.Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(original): %v", err)
	}
	if err := os.Symlink("target", path); err != nil {
		t.Fatalf("Symlink(replacement): %v", err)
	}
	replacement, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(replacement): %v", err)
	}
	if !os.SameFile(guard.entry, replacement) {
		t.Skip("filesystem did not reuse the original inode for the replacement symlink")
	}

	if err := guard.WriteFile([]byte("replacement\n"), 0o600); err == nil {
		t.Fatal("WriteFile accepted a type-swapped replacement with the retained inode")
	}
	if target, err := os.Readlink(path); err != nil || target != "target" {
		t.Fatalf("replacement symlink changed: target=%q err=%v", target, err)
	}
}

func TestPrepareReconcileLegacyMissingFile(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "from template\n")
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "run", "template-state", "test.json")

	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target:    target,
		StatePath: statePath,
		Layers: []RenderLayer{{
			Name:     "test",
			Template: template,
			Inputs:   Inputs{},
		}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()

	result := prepared.Result()
	if len(result.Changes) != 1 || result.Changes[0].Path != "managed.txt" || result.Changes[0].Kind != ChangeAdd {
		t.Fatalf("changes = %#v, want one add for managed.txt", result.Changes)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %#v, want none", result.Conflicts)
	}
}

func TestPrepareReconcileRejectsEscapingAnswersFileBeforeRender(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "from template\n")
	config := "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: ../escaped.yml\n"
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}

	_, err := PrepareReconcile(context.Background(), RenderPlan{
		Target:    filepath.Join(root, "target"),
		StatePath: filepath.Join(root, "state.json"),
		Layers:    []RenderLayer{{Name: "test", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err == nil {
		t.Fatal("PrepareReconcile succeeded with an escaping answers file")
	}
}

func TestPrepareReconcileRejectsPreservedSymlinks(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "from template\n")
	config := "_subdirectory: template\n_templates_suffix: .jinja\n_preserve_symlinks: true\n"
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}

	_, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: filepath.Join(root, "target"),
		Layers: []RenderLayer{{Name: "test", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err == nil {
		t.Fatal("PrepareReconcile succeeded with _preserve_symlinks enabled")
	}
}

func TestPrepareReconcileRejectsTemplateOutputOverlappingState(t *testing.T) {
	root := t.TempDir()
	template := writeNamedReconcileTemplate(t, root, "template", map[string]string{"state.json.jinja": "managed\n"})
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(target): %v", err)
	}
	_, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: target, StatePath: filepath.Join(target, "state.json"),
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err == nil {
		t.Fatal("PrepareReconcile accepted template output at the render state path")
	}
}

func TestPrepareReconcileConflictMatrix(t *testing.T) {
	tests := []struct {
		name          string
		old           *string
		current       *string
		new           *string
		overwrite     bool
		wantChanges   []Change
		wantConflicts []Conflict
	}{
		{
			name:        "legacy identical file is adopted",
			current:     stringPointer("new\n"),
			new:         stringPointer("new\n"),
			wantChanges: []Change{{Path: "managed.txt", Kind: ChangeAdopt}},
		},
		{
			name:          "legacy different file conflicts",
			current:       stringPointer("local\n"),
			new:           stringPointer("new\n"),
			wantConflicts: []Conflict{{Path: "managed.txt", Reason: ConflictUntrackedDifferent}},
		},
		{
			name:        "overwrite replaces legacy different file",
			current:     stringPointer("local\n"),
			new:         stringPointer("new\n"),
			overwrite:   true,
			wantChanges: []Change{{Path: "managed.txt", Kind: ChangeModify}},
		},
		{
			name:        "tracked unchanged file updates",
			old:         stringPointer("old\n"),
			current:     stringPointer("old\n"),
			new:         stringPointer("new\n"),
			wantChanges: []Change{{Path: "managed.txt", Kind: ChangeModify}},
		},
		{
			name:          "tracked locally modified file conflicts",
			old:           stringPointer("old\n"),
			current:       stringPointer("local\n"),
			new:           stringPointer("new\n"),
			wantConflicts: []Conflict{{Path: "managed.txt", Reason: ConflictLocallyModified}},
		},
		{
			name:        "tracked unchanged file deletes",
			old:         stringPointer("old\n"),
			current:     stringPointer("old\n"),
			wantChanges: []Change{{Path: "managed.txt", Kind: ChangeDelete}},
		},
		{
			name:          "tracked locally modified deletion conflicts",
			old:           stringPointer("old\n"),
			current:       stringPointer("local\n"),
			wantConflicts: []Conflict{{Path: "managed.txt", Reason: ConflictLocallyModified}},
		},
		{
			name:      "untracked user-only file is ignored",
			current:   stringPointer("user\n"),
			new:       nil,
			old:       nil,
			overwrite: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			template := writeReconcileTemplateOptional(t, root, tt.new)
			target := filepath.Join(root, "target")
			statePath := filepath.Join(root, "run", "template-state", "test.json")
			if tt.current != nil {
				writeTestFile(t, filepath.Join(target, "managed.txt"), []byte(*tt.current), 0o644)
			}
			if tt.old != nil {
				writeTestState(t, statePath, map[string]Fingerprint{
					"managed.txt": testRegularFingerprint([]byte(*tt.old), 0o644),
				})
			}

			prepared, err := PrepareReconcile(context.Background(), RenderPlan{
				Target:    target,
				StatePath: statePath,
				Layers:    []RenderLayer{{Name: "test", Template: template}},
			}, ReconcileOptions{Mode: ReconcileUpdate, Overwrite: tt.overwrite})
			if err != nil {
				t.Fatalf("PrepareReconcile: %v", err)
			}
			defer prepared.Close()

			result := prepared.Result()
			assertChanges(t, result.Changes, tt.wantChanges)
			assertConflicts(t, result.Conflicts, tt.wantConflicts)
		})
	}
}

func TestPrepareReconcileOrderedLayersAndDocuments(t *testing.T) {
	root := t.TempDir()
	first := writeNamedReconcileTemplate(t, root, "first", map[string]string{
		"shared.txt.jinja": "first\n",
		"first-only.jinja": "first only\n",
	})
	second := writeNamedReconcileTemplate(t, root, "second", map[string]string{
		"shared.txt.jinja":   "second\n",
		"service.yaml.jinja": "service:\n  name: rendered\n",
	})
	target := filepath.Join(root, "target")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target:    target,
		StatePath: filepath.Join(root, "state.json"),
		Layers: []RenderLayer{
			{Name: "first", Template: first},
			{Name: "second", Template: second},
		},
		Documents: []string{"service.yaml"},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()

	document, ok := prepared.RenderedDocument("service.yaml")
	if !ok || string(document) != "service:\n  name: rendered\n" {
		t.Fatalf("RenderedDocument(service.yaml) = %q, %v", document, ok)
	}
	assertChanges(t, prepared.Result().Changes, []Change{
		{Path: "first-only", Kind: ChangeAdd},
		{Path: "shared.txt", Kind: ChangeAdd},
	})

	rollback, err := prepared.ApplyFiles(context.Background())
	if err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	_ = rollback
	assertFileContents(t, filepath.Join(target, "shared.txt"), "second\n")
	if _, err := os.Stat(filepath.Join(target, "service.yaml")); !os.IsNotExist(err) {
		t.Fatalf("service.yaml should remain a special document, stat err = %v", err)
	}
}

func TestPreparedReconcileApplyFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	template := writeNamedReconcileTemplate(t, root, "template", map[string]string{
		"a.txt.jinja": "new a\n",
		"b.txt.jinja": "new b\n",
	})
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "state.json")
	oldState := []byte(`{"version":1}`)
	writeTestFile(t, statePath, oldState, 0o644)
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if err := os.Remove(filepath.Join(prepared.scratch, "b.txt")); err != nil {
		t.Fatalf("Remove(scratch b.txt): %v", err)
	}

	if _, err := prepared.ApplyFiles(context.Background()); err == nil {
		t.Fatal("ApplyFiles succeeded, want injected source failure")
	}
	if _, err := os.Stat(filepath.Join(target, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt remains after rollback, stat err = %v", err)
	}
	gotState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(state): %v", err)
	}
	if !bytes.Equal(gotState, oldState) {
		t.Fatalf("state changed after failed apply: %q", gotState)
	}
}

func TestPreparedReconcileRollbackRestoresDirectoryMode(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	directory := filepath.Join(target, "a")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll(directory): %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod(directory): %v", err)
	}
	writeTestFile(t, filepath.Join(directory, "keep.txt"), []byte("keep\n"), 0o600)
	template := writeNamedReconcileTemplate(t, root, "template", map[string]string{
		"a.jinja": "replacement\n",
		"z.jinja": "later\n",
	})
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate, Overwrite: true})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if err := os.Remove(filepath.Join(prepared.scratch, "z")); err != nil {
		t.Fatalf("Remove(scratch z): %v", err)
	}
	if _, err := prepared.ApplyFiles(context.Background()); err == nil {
		t.Fatal("ApplyFiles succeeded, want injected later failure")
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory): %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("restored directory mode = %o, want 700", got)
	}
	assertFileContents(t, filepath.Join(directory, "keep.txt"), "keep\n")
}

func TestPreparedReconcileDeletesDescendantsBeforeWritingAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "state.json")
	firstTemplate := writeNamedReconcileTemplate(t, root, "first-template", map[string]string{
		"a/b.txt.jinja": "old\n",
	})
	first, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: firstTemplate}},
	}, ReconcileOptions{Mode: ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile(first): %v", err)
	}
	if _, err := first.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles(first): %v", err)
	}
	if err := first.SaveState(context.Background()); err != nil {
		t.Fatalf("SaveState(first): %v", err)
	}
	_ = first.Close()

	secondTemplate := writeNamedReconcileTemplate(t, root, "second-template", map[string]string{
		"a.jinja": "new\n",
	})
	second, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: secondTemplate}},
	}, ReconcileOptions{Mode: ReconcileUpdate, Overwrite: true})
	if err != nil {
		t.Fatalf("PrepareReconcile(second): %v", err)
	}
	defer second.Close()
	if _, err := second.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles(second): %v", err)
	}
	assertFileContents(t, filepath.Join(target, "a"), "new\n")
}

func TestPreparedReconcileApplyHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "rendered\n")
	target := filepath.Join(root, "target")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prepared.ApplyFiles(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyFiles error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(target, "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("canceled apply wrote managed file, stat error = %v", err)
	}
}

func TestPreparedReconcileRejectsLiveFileChangeAfterPrepare(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	managed := filepath.Join(target, "managed.txt")
	writeTestFile(t, managed, []byte("old\n"), 0o644)
	statePath := filepath.Join(root, "state.json")
	writeTestState(t, statePath, map[string]Fingerprint{
		"managed.txt": testRegularFingerprint([]byte("old\n"), 0o644),
	})
	template := writeReconcileTemplate(t, root, "new\n")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if err := os.WriteFile(managed, []byte("edited after prepare\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(edit): %v", err)
	}
	if _, err := prepared.ApplyFiles(context.Background()); err == nil {
		t.Fatal("ApplyFiles accepted a file changed after prepare")
	}
	assertFileContents(t, managed, "edited after prepare\n")
}

func TestPreparedReconcileRejectsStateChangeAfterPrepare(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "state.json")
	writeTestState(t, statePath, map[string]Fingerprint{})
	template := writeReconcileTemplate(t, root, "new\n")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	rollback, err := prepared.ApplyFiles(context.Background())
	if err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	writeTestState(t, statePath, map[string]Fingerprint{"concurrent.txt": testRegularFingerprint([]byte("edit\n"), 0o644)})
	if err := prepared.SaveState(context.Background()); err == nil {
		t.Fatal("SaveState accepted state changed after prepare")
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("rendered file remained after rollback, stat error = %v", err)
	}
}

func TestPreparedReconcileRejectsStateCreatedAfterPrepare(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "state.json")
	template := writeReconcileTemplate(t, root, "new\n")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	writeTestState(t, statePath, map[string]Fingerprint{})
	if err := prepared.SaveState(context.Background()); err == nil {
		t.Fatal("SaveState accepted state created after prepare")
	}
}

func TestPreparedReconcileRejectsRetargetedAllowedSymlinkParent(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "rendered\n")
	target := filepath.Join(root, "target")
	expected := filepath.Join(root, "expected-source")
	attacker := filepath.Join(root, "attacker-source")
	for _, path := range []string{target, expected, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	link := filepath.Join(target, "source")
	if err := os.Symlink(expected, link); err != nil {
		t.Fatalf("Symlink(expected): %v", err)
	}
	trustedExpected := openTestTrustedRoot(t, expected)
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target:                target,
		Layers:                []RenderLayer{{Name: "template", Template: template, DestRoot: "source"}},
		AllowedSymlinkParents: map[string]*TrustedRoot{"source": trustedExpected},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if _, err := prepared.ApplyFiles(context.Background()); err == nil {
		t.Fatal("ApplyFiles succeeded through a retargeted allowed symlink parent")
	}
	if _, err := os.Stat(filepath.Join(attacker, "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("attacker destination was written, stat error = %v", err)
	}
}

func TestPreparedReconcileRollbackKeepsOriginalSourceCapability(t *testing.T) {
	root := t.TempDir()
	template := writeNamedReconcileTemplate(t, root, "template-source", map[string]string{
		"nested/managed.txt.jinja": "rendered\n",
	})
	target := filepath.Join(root, "target")
	expected := filepath.Join(root, "expected-source")
	attacker := filepath.Join(root, "attacker-source")
	for _, path := range []string{target, expected, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	link := filepath.Join(target, "source")
	if err := os.Symlink(expected, link); err != nil {
		t.Fatalf("Symlink(expected): %v", err)
	}
	trustedExpected := openTestTrustedRoot(t, expected)
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target:                target,
		Layers:                []RenderLayer{{Name: "template", Template: template, DestRoot: "source"}},
		AllowedSymlinkParents: map[string]*TrustedRoot{"source": trustedExpected},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	rollback, err := prepared.ApplyFiles(context.Background())
	if err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	assertFileContents(t, filepath.Join(expected, "nested", "managed.txt"), "rendered\n")
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(expected, "nested")); !os.IsNotExist(err) {
		t.Fatalf("expected source retained rendered parents after rollback, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(attacker, "nested")); !os.IsNotExist(err) {
		t.Fatalf("attacker source was touched during rollback, stat error = %v", err)
	}
}

func TestPrepareReconcileRejectsSymlinkedTargetAncestor(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "rendered\n")
	trusted := filepath.Join(root, "stack")
	outside := filepath.Join(root, "outside")
	for _, path := range []string{trusted, outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(trusted, "services")); err != nil {
		t.Fatalf("Symlink(services): %v", err)
	}
	_, err := PrepareReconcile(context.Background(), RenderPlan{
		TargetRoot: trusted,
		Target:     filepath.Join(trusted, "services", "agent"),
		Layers:     []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err == nil {
		t.Fatal("PrepareReconcile accepted a target beneath a symlinked ancestor")
	}
	if _, err := os.Stat(filepath.Join(outside, "agent", "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside target was written, stat error = %v", err)
	}
}

func TestGuardedTemplateSnapshotRejectsSymlinkEntries(t *testing.T) {
	root := t.TempDir()
	template := filepath.Join(root, "template")
	outside := filepath.Join(root, "outside.txt")
	if err := os.MkdirAll(template, 0o755); err != nil {
		t.Fatalf("MkdirAll(template): %v", err)
	}
	writeTestFile(t, outside, []byte("outside\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(template, "linked.txt")); err != nil {
		t.Fatalf("Symlink(linked.txt): %v", err)
	}
	guard, err := OpenGuardedPath(root, root, "template", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath: %v", err)
	}
	defer guard.Close()
	if _, _, err := guard.SnapshotDirectory(context.Background()); err == nil {
		t.Fatal("SnapshotDirectory accepted a template containing a symlink")
	}
}

func TestAllowedSymlinkParentDoesNotRecanonicalizeChangedSource(t *testing.T) {
	root := t.TempDir()
	expected := filepath.Join(root, "expected")
	attacker := filepath.Join(root, "attacker")
	for _, path := range []string{expected, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	trustedExpected := openTestTrustedRoot(t, expected)
	original := expected + "-moved"
	if err := os.Rename(expected, original); err != nil {
		t.Fatalf("Rename(expected): %v", err)
	}
	if err := os.Symlink(attacker, expected); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if err := trustedExpected.VerifyPath(expected); err == nil {
		t.Fatal("trusted root accepted a source path replaced by a symlink")
	}
}

func TestGuardedPathAnchorsVerifiedParentAcrossRetarget(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "workspaces", "feature")
	attacker := filepath.Join(root, "attacker")
	for _, path := range []string{original, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	guard, err := OpenGuardedPath(root, root, "workspaces/feature/managed.txt", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath: %v", err)
	}
	defer guard.Close()
	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("Rename(original): %v", err)
	}
	if err := os.Symlink(attacker, original); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if err := guard.WriteFile([]byte("managed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertFileContents(t, filepath.Join(moved, "managed.txt"), "managed\n")
	if _, err := os.Stat(filepath.Join(attacker, "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("retargeted parent was written, stat error = %v", err)
	}
}

func TestGuardedPathVerifyPathIdentityRejectsInstalledDirectorySwap(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatalf("MkdirAll(stage): %v", err)
	}
	writeTestFile(t, filepath.Join(stage, "marker.txt"), []byte("original\n"), 0o644)
	guard, err := OpenGuardedPath(root, root, "dest", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath: %v", err)
	}
	defer guard.Close()
	if err := guard.ReplaceFrom(context.Background(), stage); err != nil {
		t.Fatalf("ReplaceFrom: %v", err)
	}
	if err := os.Rename(dest, dest+"-moved"); err != nil {
		t.Fatalf("Rename(dest): %v", err)
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := guard.VerifyPathIdentity(dest); err == nil {
		t.Fatal("VerifyPathIdentity accepted a replacement directory")
	}
}

func TestGuardedPathReplaceFromCleansPartialOutput(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatalf("MkdirAll(stage): %v", err)
	}
	writeTestFile(t, filepath.Join(stage, "a.txt"), []byte("partial\n"), 0o644)
	if err := syscall.Mkfifo(filepath.Join(stage, "z-fifo"), 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	guard, err := OpenGuardedPath(root, root, "nested/dest", nil)
	if err != nil {
		t.Fatalf("OpenGuardedPath: %v", err)
	}
	defer guard.Close()
	if err := guard.ReplaceFrom(context.Background(), stage); err == nil {
		t.Fatal("ReplaceFrom accepted an unsupported staged entry")
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatalf("partial destination parents remained, stat error = %v", err)
	}
}

func TestTrustedRootRejectsPathReplacedByRealDirectory(t *testing.T) {
	root := t.TempDir()
	expected := filepath.Join(root, "source")
	if err := os.Mkdir(expected, 0o755); err != nil {
		t.Fatalf("Mkdir(expected): %v", err)
	}
	trusted, err := OpenTrustedRoot(expected)
	if err != nil {
		t.Fatalf("OpenTrustedRoot: %v", err)
	}
	defer trusted.Close()
	if err := os.Rename(expected, expected+"-moved"); err != nil {
		t.Fatalf("Rename(expected): %v", err)
	}
	if err := os.Mkdir(expected, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := trusted.VerifyPath(expected); err == nil {
		t.Fatal("VerifyPath accepted a replacement real directory")
	}
}

func TestGuardedPathRejectsMissingAncestorBeforeDeclaredSourceLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
	source := filepath.Join(root, "source")
	for _, path := range []string{target, source} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("Mkdir(%s): %v", path, err)
		}
	}
	trusted := openTestTrustedRoot(t, source)
	if _, err := OpenGuardedPath(root, target, "nested/app/managed.txt", map[string]*TrustedRoot{"nested/app": trusted}); err == nil {
		t.Fatal("OpenGuardedPath accepted a missing ancestor before a declared source link")
	}
	if _, err := os.Stat(filepath.Join(target, "nested")); !os.IsNotExist(err) {
		t.Fatalf("missing source ancestors were created, stat error = %v", err)
	}
}

func TestPreparedReconcileRejectsSymlinkedStateParent(t *testing.T) {
	root := t.TempDir()
	template := writeReconcileTemplate(t, root, "rendered\n")
	target := filepath.Join(root, "target")
	stateRoot := filepath.Join(root, "trusted")
	outside := filepath.Join(root, "outside")
	for _, path := range []string{target, stateRoot, outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: stateRoot, StatePath: filepath.Join(stateRoot, "run", "state.json"),
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if err := os.Symlink(outside, filepath.Join(stateRoot, "run")); err != nil {
		t.Fatalf("Symlink(state parent): %v", err)
	}
	if err := prepared.SaveState(context.Background()); err == nil {
		t.Fatal("SaveState succeeded through a symlinked state parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("outside state path was written, stat error = %v", err)
	}
}

func TestRemoveRenderStateRootedRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "trusted")
	outside := filepath.Join(root, "outside")
	for _, path := range []string{stateRoot, outside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	outsideState := filepath.Join(outside, "state.json")
	writeTestFile(t, outsideState, []byte("keep\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(stateRoot, "run")); err != nil {
		t.Fatalf("Symlink(state parent): %v", err)
	}
	if err := RemoveRenderStateRooted(stateRoot, filepath.Join(stateRoot, "run", "state.json")); err == nil {
		t.Fatal("RemoveRenderStateRooted succeeded through a symlinked parent")
	}
	assertFileContents(t, outsideState, "keep\n")
}

func TestPreparedReconcilePreservesPreviousProtectedRootOnDeletion(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	managed := filepath.Join(target, "persist", "managed.txt")
	writeTestFile(t, managed, []byte("old\n"), 0o644)
	statePath := filepath.Join(root, "state.json")
	state := RenderState{
		Version: renderStateVersion,
		Files: map[string]Fingerprint{
			"persist":             {Kind: fingerprintDirectory, Mode: 0o755},
			"persist/managed.txt": testRegularFingerprint([]byte("old\n"), 0o644),
		},
		ProtectedPaths: []string{"persist"},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal(state): %v", err)
	}
	writeTestFile(t, statePath, data, 0o644)

	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	if info, err := os.Stat(filepath.Join(target, "persist")); err != nil || !info.IsDir() {
		t.Fatalf("protected persist root was removed: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed file was not deleted, stat error = %v", err)
	}
}

func TestPreparedReconcilePrunesEmptyManagedParentsAtCommit(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	managed := filepath.Join(target, "nested", "managed.txt")
	writeTestFile(t, managed, []byte("old\n"), 0o644)
	statePath := filepath.Join(root, "state.json")
	writeTestState(t, statePath, map[string]Fingerprint{
		"nested/managed.txt": testRegularFingerprint([]byte("old\n"), 0o644),
	})
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StateRoot: root, StatePath: statePath,
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	if info, err := os.Stat(filepath.Join(target, "nested")); err != nil || !info.IsDir() {
		t.Fatalf("parent was pruned before commit: info=%v err=%v", info, err)
	}
	if err := prepared.SaveState(context.Background()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "nested")); !os.IsNotExist(err) {
		t.Fatalf("empty managed parent remained after commit, stat error = %v", err)
	}
}

func TestPreparedReconcileCommitsStateThroughPreparedRootCapability(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stack")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root): %v", err)
	}
	template := writeReconcileTemplate(t, base, "rendered\n")
	statePath := filepath.Join(root, "run", "template-state", "stack.json")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: root, TargetRoot: root, StateRoot: root, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("Rename(root): %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := prepared.SaveState(context.Background()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "run", "template-state", "stack.json")); err != nil {
		t.Fatalf("prepared root state missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !os.IsNotExist(err) {
		t.Fatalf("replacement root received state, stat error = %v", err)
	}
}

func TestPreparedReconcileAppliesThroughPreparedTargetRootCapability(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stack")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root): %v", err)
	}
	template := writeReconcileTemplate(t, base, "rendered\n")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: root, TargetRoot: root,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("Rename(root): %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	assertFileContents(t, filepath.Join(moved, "managed.txt"), "rendered\n")
	if _, err := os.Stat(filepath.Join(root, "managed.txt")); !os.IsNotExist(err) {
		t.Fatalf("replacement root received output, stat error = %v", err)
	}
}

func TestPreparedReconcileSharesNestedStateRootWithTargetWrites(t *testing.T) {
	project := t.TempDir()
	stackRoot := filepath.Join(project, ".angee")
	if err := os.Mkdir(stackRoot, 0o755); err != nil {
		t.Fatalf("Mkdir(stack root): %v", err)
	}
	template := writeNamedReconcileTemplate(t, project, "template-source", map[string]string{
		".angee/managed.txt.jinja": "rendered\n",
	})
	statePath := filepath.Join(stackRoot, "run", "template-state", "stack.json")
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: project, TargetRoot: project, StateRoot: stackRoot, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	moved := stackRoot + "-moved"
	if err := os.Rename(stackRoot, moved); err != nil {
		t.Fatalf("Rename(stack root): %v", err)
	}
	if err := os.Mkdir(stackRoot, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	if err := prepared.SaveState(context.Background()); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	assertFileContents(t, filepath.Join(moved, "managed.txt"), "rendered\n")
	if _, err := os.Stat(filepath.Join(moved, "run", "template-state", "stack.json")); err != nil {
		t.Fatalf("nested retained state missing: %v", err)
	}
	if entries, err := os.ReadDir(stackRoot); err != nil || len(entries) != 0 {
		t.Fatalf("replacement nested root was modified: entries=%v err=%v", entries, err)
	}
}

func TestPreparedReconcilePoolsApplyParentCapabilities(t *testing.T) {
	root := t.TempDir()
	files := make(map[string]string, 128)
	for index := 0; index < 128; index++ {
		files[fmt.Sprintf("file-%03d.txt.jinja", index)] = "rendered\n"
	}
	template := writeNamedReconcileTemplate(t, root, "template-source", files)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(target): %v", err)
	}
	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	if got := len(prepared.applyParentRoots); got != 1 {
		t.Fatalf("retained apply parent roots = %d, want 1 for flat output", got)
	}
	for _, guard := range prepared.applyGuards {
		if len(guard.roots) != 0 {
			t.Fatalf("apply guard retained %d private parent root(s), want shared capability", len(guard.roots))
		}
	}
}

func TestPreparedReconcileDryRunAndDeterministicState(t *testing.T) {
	root := t.TempDir()
	template := writeNamedReconcileTemplate(t, root, "template", map[string]string{
		"z.txt.jinja":        "z\n",
		"a.txt.jinja":        "a\n",
		"service.yaml.jinja": "name: demo\n",
	})
	target := filepath.Join(root, "target")
	statePath := filepath.Join(root, "state.json")
	plan := RenderPlan{
		Target: target, StatePath: statePath,
		Layers:    []RenderLayer{{Name: "template", Template: template}},
		Documents: []string{"service.yaml"},
	}
	dryRun, err := PrepareReconcile(context.Background(), plan, ReconcileOptions{Mode: ReconcileUpdate, DryRun: true})
	if err != nil {
		t.Fatalf("PrepareReconcile(dry-run): %v", err)
	}
	if _, err := dryRun.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles(dry-run): %v", err)
	}
	if err := dryRun.SaveState(context.Background()); err != nil {
		t.Fatalf("SaveState(dry-run): %v", err)
	}
	dryRun.Close()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run target exists, stat err = %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("dry-run state exists, stat err = %v", err)
	}

	apply := func() []byte {
		prepared, err := PrepareReconcile(context.Background(), plan, ReconcileOptions{Mode: ReconcileUpdate})
		if err != nil {
			t.Fatalf("PrepareReconcile: %v", err)
		}
		defer prepared.Close()
		if _, err := prepared.ApplyFiles(context.Background()); err != nil {
			t.Fatalf("ApplyFiles: %v", err)
		}
		if err := prepared.SaveState(context.Background()); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("ReadFile(state): %v", err)
		}
		return data
	}
	first := apply()
	secondBytes := apply()
	if !bytes.Equal(first, secondBytes) {
		t.Fatalf("state writes are not deterministic:\nfirst: %s\nsecond: %s", first, secondBytes)
	}
}

func TestReadRenderStateRejectsCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeTestFile(t, path, []byte("{not-json"), 0o644)
	if _, _, err := ReadRenderState(path); err == nil {
		t.Fatal("ReadRenderState succeeded for corrupt JSON")
	}
}

func TestPrepareReconcileTracksExecutableMode(t *testing.T) {
	root := t.TempDir()
	body := "#!/bin/sh\n"
	template := writeReconcileTemplateOptional(t, root, &body)
	if err := os.Chmod(filepath.Join(template, "template", "managed.txt.jinja"), 0o755); err != nil {
		t.Fatalf("Chmod(template): %v", err)
	}
	target := filepath.Join(root, "target")
	writeTestFile(t, filepath.Join(target, "managed.txt"), []byte(body), 0o644)
	statePath := filepath.Join(root, "state.json")
	writeTestState(t, statePath, map[string]Fingerprint{"managed.txt": testRegularFingerprint([]byte(body), 0o644)})

	prepared, err := PrepareReconcile(context.Background(), RenderPlan{
		Target: target, StatePath: statePath,
		Layers: []RenderLayer{{Name: "template", Template: template}},
	}, ReconcileOptions{Mode: ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	assertChanges(t, prepared.Result().Changes, []Change{{Path: "managed.txt", Kind: ChangeModify}})
	if _, err := prepared.ApplyFiles(context.Background()); err != nil {
		t.Fatalf("ApplyFiles: %v", err)
	}
	info, err := os.Stat(filepath.Join(target, "managed.txt"))
	if err != nil {
		t.Fatalf("Stat(managed.txt): %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("managed.txt mode = %o, want 755", info.Mode().Perm())
	}
}

func TestReconcilePathDetectsSymlinkTargetConflict(t *testing.T) {
	old := Fingerprint{Kind: fingerprintSymlink, Link: "old-target"}
	current := Fingerprint{Kind: fingerprintSymlink, Link: "local-target"}
	newFingerprint := Fingerprint{Kind: fingerprintSymlink, Link: "new-target"}
	change, conflict := reconcilePath("managed-link", old, true, current, true, newFingerprint, true, false)
	if change != nil {
		t.Fatalf("change = %#v, want nil", change)
	}
	want := Conflict{Path: "managed-link", Reason: ConflictLocallyModified}
	if conflict == nil || *conflict != want {
		t.Fatalf("conflict = %#v, want %#v", conflict, want)
	}
}

func writeReconcileTemplateOptional(t *testing.T, root string, body *string) string {
	t.Helper()
	template := filepath.Join(root, "template-source")
	if err := os.MkdirAll(filepath.Join(template, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template): %v", err)
	}
	config := "_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n"
	writeTestFile(t, filepath.Join(template, "copier.yml"), []byte(config), 0o644)
	if body != nil {
		writeTestFile(t, filepath.Join(template, "template", "managed.txt.jinja"), []byte(*body), 0o644)
	}
	return template
}

func writeNamedReconcileTemplate(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	template := filepath.Join(root, name)
	writeTestFile(t, filepath.Join(template, "copier.yml"), []byte("_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n"), 0o644)
	for path, body := range files {
		writeTestFile(t, filepath.Join(template, "template", filepath.FromSlash(path)), []byte(body), 0o644)
	}
	return template
}

func writeTestState(t *testing.T, path string, files map[string]Fingerprint) {
	t.Helper()
	data, err := json.Marshal(RenderState{Version: renderStateVersion, Files: files})
	if err != nil {
		t.Fatalf("Marshal(state): %v", err)
	}
	writeTestFile(t, path, data, 0o644)
}

func writeTestFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func testRegularFingerprint(data []byte, mode fs.FileMode) Fingerprint {
	sum := sha256.Sum256(data)
	return Fingerprint{Kind: fingerprintRegular, SHA256: hex.EncodeToString(sum[:]), Mode: mode.Perm()}
}

func stringPointer(value string) *string { return &value }

func assertChanges(t *testing.T, got, want []Change) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("changes = %#v, want %#v", got, want)
		}
	}
}

func assertConflicts(t *testing.T, got, want []Conflict) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("conflicts = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("conflicts = %#v, want %#v", got, want)
		}
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
