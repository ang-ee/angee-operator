package copierx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
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

	rollback, err := prepared.ApplyFiles()
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

	if _, err := prepared.ApplyFiles(); err == nil {
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
	if _, err := dryRun.ApplyFiles(); err != nil {
		t.Fatalf("ApplyFiles(dry-run): %v", err)
	}
	if err := dryRun.SaveState(); err != nil {
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
		if _, err := prepared.ApplyFiles(); err != nil {
			t.Fatalf("ApplyFiles: %v", err)
		}
		if err := prepared.SaveState(); err != nil {
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
	if _, err := prepared.ApplyFiles(); err != nil {
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
