package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
)

func trustedRootForServiceTest(t *testing.T, path string) *copierx.TrustedRoot {
	t.Helper()
	root, err := copierx.OpenTrustedRoot(path)
	if err != nil {
		t.Fatalf("OpenTrustedRoot(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestJoinRollbackErrorsPreservesPrimaryAndRollbackFailures(t *testing.T) {
	primary := errors.New("primary")
	rollback := errors.New("rollback")
	err := joinRollbackErrors(primary, func() error { return rollback })
	if !errors.Is(err, primary) || !errors.Is(err, rollback) {
		t.Fatalf("joined error = %v, want both primary and rollback failures", err)
	}
}

func TestApplyRenderedDocumentsRejectsRetargetedAllowedSymlinkParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
	expected := filepath.Join(root, "expected-source")
	attacker := filepath.Join(root, "attacker-source")
	for _, path := range []string{target, expected, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	if err := os.Symlink(attacker, filepath.Join(target, "source")); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	allowed := map[string]*copierx.TrustedRoot{"source": trustedRootForServiceTest(t, expected)}
	_, _, _, err := applyRenderedDocuments(context.Background(), targetPathOpener(target, target, allowed), target, map[string][]byte{
		"source/angee.yaml": []byte("version: 1\n"),
	}, nil, nil, nil, false)
	if err == nil {
		t.Fatal("applyRenderedDocuments succeeded through a retargeted source symlink")
	}
	if _, err := os.Stat(filepath.Join(attacker, "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("attacker target was written, stat error = %v", err)
	}
}

func TestApplyRenderedDocumentsBackupFailurePreservesExistingEntry(t *testing.T) {
	target := t.TempDir()
	documentDir := filepath.Join(target, "angee.yaml")
	if err := os.MkdirAll(documentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(document): %v", err)
	}
	marker := filepath.Join(documentDir, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(marker): %v", err)
	}
	_, _, _, err := applyRenderedDocuments(context.Background(), targetPathOpener(target, target, nil), target, map[string][]byte{
		"angee.yaml": []byte("new\n"),
	}, nil, nil, nil, false)
	if err == nil {
		t.Fatal("applyRenderedDocuments accepted a directory document destination")
	}
	data, readErr := os.ReadFile(marker)
	if readErr != nil || string(data) != "keep\n" {
		t.Fatalf("existing destination changed after backup failure: data=%q err=%v", data, readErr)
	}
}

func TestApplyRenderedDocumentsRejectsChangedMergeBaseline(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(target, "angee.yaml")
	if err := os.WriteFile(path, []byte("edited after merge\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(current): %v", err)
	}
	_, _, _, err := applyRenderedDocuments(context.Background(), targetPathOpener(target, target, nil), target, map[string][]byte{
		"angee.yaml": []byte("merged\n"),
	}, nil, nil, map[string]renderedDocumentExpectation{
		"angee.yaml": {Data: []byte("merge baseline\n"), Exists: true},
	}, false)
	if err == nil {
		t.Fatal("applyRenderedDocuments accepted a changed merge baseline")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "edited after merge\n" {
		t.Fatalf("changed document was overwritten: data=%q err=%v", data, readErr)
	}
}

func TestApplyRenderedDocumentsRejectsReplacedMergeBaselineWithSameContents(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(target, "angee.yaml")
	if err := os.WriteFile(path, []byte("baseline\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(baseline): %v", err)
	}
	opener := targetPathOpener(target, target, nil)
	expectations, err := captureRenderedDocumentExpectations(context.Background(), opener, map[string][]byte{"angee.yaml": []byte("merged\n")})
	if err != nil {
		t.Fatalf("captureRenderedDocumentExpectations: %v", err)
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatalf("Rename(baseline): %v", err)
	}
	if err := os.WriteFile(path, []byte("baseline\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement): %v", err)
	}
	if _, _, _, err := applyRenderedDocuments(context.Background(), opener, target, map[string][]byte{"angee.yaml": []byte("merged\n")}, nil, nil, expectations, false); err == nil {
		t.Fatal("applyRenderedDocuments accepted a same-content replacement baseline")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "baseline\n" {
		t.Fatalf("replacement document changed: data=%q err=%v", data, err)
	}
}

func TestTemplateDocumentsAndPersistPathsUsePreparedTargetRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "stack")
	template := filepath.Join(base, "template")
	if err := os.MkdirAll(filepath.Join(template, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template): %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(target): %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "angee.yaml"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(old document): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte("_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "angee.yaml.jinja"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(document template): %v", err)
	}
	prepared, err := copierx.PrepareReconcile(context.Background(), copierx.RenderPlan{
		Target: target, TargetRoot: target,
		Layers:    []copierx.RenderLayer{{Name: "template", Template: template}},
		Documents: []string{"angee.yaml"},
	}, copierx.ReconcileOptions{Mode: copierx.ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	moved := target + "-moved"
	if err := os.Rename(target, moved); err != nil {
		t.Fatalf("Rename(target): %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	expectations, err := captureRenderedDocumentExpectations(context.Background(), prepared.OpenTargetPath, map[string][]byte{"angee.yaml": []byte("new\n")})
	if err != nil {
		t.Fatalf("captureRenderedDocumentExpectations: %v", err)
	}
	rollbackDocuments, closeDocuments, verifyDocuments, err := applyRenderedDocuments(context.Background(), prepared.OpenTargetPath, target, map[string][]byte{"angee.yaml": []byte("new\n")}, nil, nil, expectations, false)
	if err != nil {
		t.Fatalf("applyRenderedDocuments: %v", err)
	}
	defer closeDocuments()
	if err := verifyDocuments(); err == nil {
		t.Fatal("verifyDocuments accepted the replacement public target")
	}
	rollbackPersist, closePersist, _, err := materializePersistPaths(context.Background(), prepared.OpenTargetPath, target, map[string]manifest.PersistPath{"cache": {Subpath: "cache"}}, nil)
	if err != nil {
		t.Fatalf("materializePersistPaths: %v", err)
	}
	defer closePersist()
	if data, err := os.ReadFile(filepath.Join(moved, "angee.yaml")); err != nil || string(data) != "new\n" {
		t.Fatalf("prepared target document = %q, %v; want new", data, err)
	}
	if info, err := os.Stat(filepath.Join(moved, "cache")); err != nil || !info.IsDir() {
		t.Fatalf("prepared target persist path: info=%v err=%v", info, err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("replacement target was modified: entries=%v err=%v", entries, err)
	}
	if err := rollbackPersist(); err != nil {
		t.Fatalf("rollback persist: %v", err)
	}
	if err := rollbackDocuments(); err != nil {
		t.Fatalf("rollback documents: %v", err)
	}
}

func TestStagedSourceCloneUsesPreparedTargetRootAndRollsBack(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "stack")
	template := filepath.Join(base, "template")
	remote := filepath.Join(base, "remote.git")
	if err := os.MkdirAll(filepath.Join(template, "template"), 0o755); err != nil {
		t.Fatalf("MkdirAll(template): %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(target): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte("_subdirectory: template\n_templates_suffix: .jinja\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	seedWorktreeRemote(t, base, remote)
	prepared, err := copierx.PrepareReconcile(context.Background(), copierx.RenderPlan{
		Target: target, TargetRoot: target,
		Layers: []copierx.RenderLayer{{Name: "template", Template: template}},
	}, copierx.ReconcileOptions{Mode: copierx.ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	moved := target + "-moved"
	if err := os.Rename(target, moved); err != nil {
		t.Fatalf("Rename(target): %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	platform, err := New(target)
	if err != nil {
		t.Fatalf("New(target): %v", err)
	}
	stack := &manifest.Stack{Sources: map[string]manifest.Source{
		"app": {Kind: "git", Repo: remote, DefaultRef: "main"},
	}}
	rollback, closeSources, verifySources, err := platform.stageReferencedSources(context.Background(), stack, preparedAbsolutePathOpener(prepared, target))
	if err != nil {
		t.Fatalf("stageReferencedSources: %v", err)
	}
	defer closeSources()
	if _, err := os.Stat(filepath.Join(moved, "sources", "app", ".git")); err != nil {
		t.Fatalf("prepared target clone missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "sources")); !os.IsNotExist(err) {
		t.Fatalf("replacement target received source clone, stat error = %v", err)
	}
	if err := verifySources(); err == nil {
		t.Fatal("verifySources accepted a replaced public target")
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback staged source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "sources")); !os.IsNotExist(err) {
		t.Fatalf("staged source parents remain after rollback, stat error = %v", err)
	}
}

func TestParentStackTransactionRetainsOriginalRootForSaveAndRollback(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stack")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root): %v", err)
	}
	original := []byte("version: 1\nkind: stack\nname: original\n")
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), original, 0o644); err != nil {
		t.Fatalf("WriteFile(manifest): %v", err)
	}
	tx, stack, err := openParentStackTransaction(root, false)
	if err != nil {
		t.Fatalf("openParentStackTransaction: %v", err)
	}
	defer tx.Close()
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("Rename(root): %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := tx.VerifyRootPath(root); err == nil {
		t.Fatal("VerifyRootPath accepted a replacement root")
	}
	stack.Name = "updated"
	if err := tx.Save(stack); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("replacement root received manifest, stat error = %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(moved, "angee.yaml"))
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("restored manifest = %q, %v; want original", data, err)
	}
}

func TestParentStackTransactionRejectsPreparedRootABA(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stack")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root A): %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "angee.yaml"), []byte("version: 1\nkind: stack\nname: a\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest A): %v", err)
	}
	tx, _, err := openParentStackTransaction(root, false)
	if err != nil {
		t.Fatalf("openParentStackTransaction: %v", err)
	}
	defer tx.Close()
	rootA := root + "-a"
	if err := os.Rename(root, rootA); err != nil {
		t.Fatalf("Rename(root A): %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(root B): %v", err)
	}
	prepared, err := copierx.PrepareReconcile(context.Background(), copierx.RenderPlan{
		Target: root, TargetRoot: root, StateRoot: root, StatePath: filepath.Join(root, "state.json"),
	}, copierx.ReconcileOptions{Mode: copierx.ReconcileUpdate})
	if err != nil {
		t.Fatalf("PrepareReconcile(root B): %v", err)
	}
	defer prepared.Close()
	if err := os.Rename(root, root+"-b"); err != nil {
		t.Fatalf("Rename(root B): %v", err)
	}
	if err := os.Rename(rootA, root); err != nil {
		t.Fatalf("Restore(root A): %v", err)
	}
	if err := tx.VerifyRootPath(root); err != nil {
		t.Fatalf("pathname verification should observe restored root A: %v", err)
	}
	if err := tx.VerifyPreparedRoot(root, prepared); err == nil {
		t.Fatal("VerifyPreparedRoot accepted independently retained root B after ABA")
	}
}

func TestApplyRenderedDocumentsRollbackRemovesCreatedParents(t *testing.T) {
	target := t.TempDir()
	rollback, closeDocuments, _, err := applyRenderedDocuments(context.Background(), targetPathOpener(target, target, nil), target, map[string][]byte{
		"nested/angee.yaml": []byte("new\n"),
	}, nil, nil, nil, false)
	if err != nil {
		t.Fatalf("applyRenderedDocuments: %v", err)
	}
	defer closeDocuments()
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "nested")); !os.IsNotExist(err) {
		t.Fatalf("created parent remained after rollback, stat error = %v", err)
	}
}

func TestApplyRenderedDocumentsDeletesAndRestoresRuntimeArtifact(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(target, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte("old runtime\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(runtime): %v", err)
	}
	opener := targetPathOpener(target, target, nil)
	expectations, err := captureRenderedDocumentExpectations(context.Background(), opener, map[string][]byte{"docker-compose.yaml": nil})
	if err != nil {
		t.Fatalf("captureRenderedDocumentExpectations: %v", err)
	}
	rollback, closeDocuments, verifyDocuments, err := applyRenderedDocuments(
		context.Background(), opener, target, nil,
		map[string]bool{"docker-compose.yaml": true}, nil, expectations, false,
	)
	if err != nil {
		t.Fatalf("applyRenderedDocuments(delete): %v", err)
	}
	defer closeDocuments()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("runtime artifact remains after deletion, stat error = %v", err)
	}
	if err := verifyDocuments(); err != nil {
		t.Fatalf("verify deleted runtime artifact: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback deleted runtime artifact: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "old runtime\n" {
		t.Fatalf("restored runtime artifact = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restored runtime mode = %v, %v; want 0600", info, err)
	}
}

func TestApplyRenderedDocumentsRollbackKeepsOriginalSourceCapability(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
	expected := filepath.Join(root, "expected-source")
	attacker := filepath.Join(root, "attacker-source")
	for _, path := range []string{target, expected, attacker} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	manifestPath := filepath.Join(expected, "angee.yaml")
	if err := os.WriteFile(manifestPath, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(old): %v", err)
	}
	link := filepath.Join(target, "source")
	if err := os.Symlink(expected, link); err != nil {
		t.Fatalf("Symlink(expected): %v", err)
	}
	trustedExpected := trustedRootForServiceTest(t, expected)
	allowed := map[string]*copierx.TrustedRoot{"source": trustedExpected}
	rollback, closeDocuments, _, err := applyRenderedDocuments(context.Background(), targetPathOpener(target, target, allowed), target, map[string][]byte{
		"source/angee.yaml": []byte("new\n"),
	}, nil, nil, nil, false)
	if err != nil {
		t.Fatalf("applyRenderedDocuments: %v", err)
	}
	defer closeDocuments()
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil || string(data) != "old\n" {
		t.Fatalf("restored document = %q, %v; want old", data, err)
	}
	if _, err := os.Stat(filepath.Join(attacker, "angee.yaml")); !os.IsNotExist(err) {
		t.Fatalf("attacker source was touched during rollback, stat error = %v", err)
	}
}

func TestMaterializePersistPathsVerifierRejectsExistingDirectoryReplacement(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(target, "cache")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir(cache): %v", err)
	}
	_, closePersist, verifyPersist, err := materializePersistPaths(
		context.Background(), targetPathOpener(target, target, nil), target,
		map[string]manifest.PersistPath{"cache": {Subpath: "cache"}}, nil,
	)
	if err != nil {
		t.Fatalf("materializePersistPaths: %v", err)
	}
	defer closePersist()
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatalf("Rename(cache): %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := verifyPersist(); err == nil {
		t.Fatal("verifyPersist accepted an existing persist directory replacement")
	}
}

func TestMaterializePersistPathsRollbackKeepsOriginalSourceCapability(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
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
	trustedExpected := trustedRootForServiceTest(t, expected)
	allowed := map[string]*copierx.TrustedRoot{"source": trustedExpected}
	rollback, closePersistPaths, _, err := materializePersistPaths(context.Background(), targetPathOpener(target, target, allowed), target, map[string]manifest.PersistPath{
		"cache": {Subpath: "source/cache/nested"},
	}, allowed)
	if err != nil {
		t.Fatalf("materializePersistPaths: %v", err)
	}
	defer closePersistPaths()
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove(link): %v", err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatalf("Symlink(attacker): %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(expected, "cache")); !os.IsNotExist(err) {
		t.Fatalf("expected source retained persist directories, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(attacker, "cache")); !os.IsNotExist(err) {
		t.Fatalf("attacker source was touched during rollback, stat error = %v", err)
	}
}

func TestMaterializePersistPathEqualToLocalSourceRoot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	source := filepath.Join(root, "source")
	for _, path := range []string{workspace, source} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	if err := os.Symlink(source, filepath.Join(workspace, "app")); err != nil {
		t.Fatalf("Symlink(source): %v", err)
	}
	trusted := trustedRootForServiceTest(t, source)
	allowed := map[string]*copierx.TrustedRoot{"app": trusted}
	rollback, closePaths, verifyPaths, err := materializePersistPaths(context.Background(), targetPathOpener(workspace, workspace, allowed), workspace, map[string]manifest.PersistPath{
		"app": {Subpath: "app"},
	}, allowed)
	if err != nil {
		t.Fatalf("materializePersistPaths: %v", err)
	}
	defer closePaths()
	if err := os.Rename(source, source+"-old"); err != nil {
		t.Fatalf("Rename(source): %v", err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement source): %v", err)
	}
	if err := verifyPaths(); err == nil {
		t.Fatal("verifyPaths accepted a replaced local Source target")
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(workspace, "app")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("local source link changed: info=%v err=%v", info, err)
	}
}

func TestPreparedAbsolutePathOpenerOpensTargetRootSource(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "stack")
	template := filepath.Join(base, "template")
	sibling := filepath.Join(base, "framework")
	for _, path := range []string{filepath.Join(template, "template"), target, sibling} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(template, "copier.yml"), []byte("_subdirectory: template\n_templates_suffix: .jinja\n_answers_file: .copier-answers.yml\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(template, "template", "managed.txt.jinja"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(template body): %v", err)
	}
	prepared, err := copierx.PrepareReconcile(context.Background(), copierx.RenderPlan{
		Target: target, TargetRoot: target,
		Layers: []copierx.RenderLayer{{Name: "template", Template: template}},
	}, copierx.ReconcileOptions{Mode: copierx.ReconcileCreate})
	if err != nil {
		t.Fatalf("PrepareReconcile: %v", err)
	}
	defer prepared.Close()
	opener := preparedAbsolutePathOpener(prepared, target)

	// A local source whose path IS the render target root (a `framework` source
	// at the repo root in the repo layout) must open through the absolute opener,
	// not fail with "escapes render target".
	dest, err := opener(target)
	if err != nil {
		t.Fatalf("opener(target root): %v", err)
	}
	info, exists, err := dest.Lstat()
	_ = dest.Close()
	if err != nil || !exists || !info.IsDir() {
		t.Fatalf("target-root source: info=%v exists=%v err=%v", info, exists, err)
	}

	// A source outside the target still routes to the absolute opener.
	outside, err := opener(sibling)
	if err != nil {
		t.Fatalf("opener(sibling): %v", err)
	}
	_ = outside.Close()
}

func TestStagedLocalSourceVerifierRejectsSymlinkTargetReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "stack")
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(source): %v", err)
	}
	link := filepath.Join(root, "local-source")
	if err := os.Symlink(source, link); err != nil {
		t.Fatalf("Symlink(source): %v", err)
	}
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New(root): %v", err)
	}
	stack := &manifest.Stack{Sources: map[string]manifest.Source{
		"app": {Kind: "local", Path: link},
	}}
	_, closeSources, verifySources, err := platform.stageReferencedSources(context.Background(), stack, openAbsoluteGuardedPath)
	if err != nil {
		t.Fatalf("stageReferencedSources: %v", err)
	}
	defer closeSources()
	if err := os.Rename(source, source+"-old"); err != nil {
		t.Fatalf("Rename(source): %v", err)
	}
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement): %v", err)
	}
	if err := verifySources(); err == nil {
		t.Fatal("verifySources accepted an unchanged symlink to a replaced target")
	}
}

func TestResolveWorkspaceChainTemplateSnapshotsGuardedSource(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".angee")
	workspace := filepath.Join(root, "workspaces", "feature")
	source := filepath.Join(base, "source")
	template := filepath.Join(source, ".templates", "stacks", "dev")
	for _, path := range []string{workspace, filepath.Join(template, "template")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	configPath := filepath.Join(template, "copier.yml")
	if err := os.WriteFile(configPath, []byte("_subdirectory: template\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(copier.yml): %v", err)
	}
	if err := os.Symlink(source, filepath.Join(workspace, "app")); err != nil {
		t.Fatalf("Symlink(source): %v", err)
	}
	trustedSource := trustedRootForServiceTest(t, source)
	platform, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snapshot, _, cleanup, err := platform.resolveWorkspaceChainTemplate(context.Background(), workspace, "app/.templates/stacks/dev", map[string]*copierx.TrustedRoot{"app": trustedSource})
	if err != nil {
		t.Fatalf("resolveWorkspaceChainTemplate: %v", err)
	}
	if cleanup == nil {
		t.Fatal("resolveWorkspaceChainTemplate returned no snapshot cleanup")
	}
	defer cleanup()
	if err := os.WriteFile(configPath, []byte("changed: true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(changed copier.yml): %v", err)
	}
	config, err := os.ReadFile(filepath.Join(snapshot, "copier.yml"))
	if err != nil {
		t.Fatalf("ReadFile(snapshot copier.yml): %v", err)
	}
	if string(config) != "_subdirectory: template\n" {
		t.Fatalf("snapshot changed after source modification: %q", config)
	}
}
