package cli

import (
	"bytes"
	"testing"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

func TestJobRunPrintsBufferedOutputOnceWithoutOperatorSink(t *testing.T) {
	root := t.TempDir()
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "cli-job-output-test",
		Jobs: map[string]manifest.Job{
			"codegen": {
				Runtime: manifest.RuntimeLocal,
				Command: []string{"sh", "-c", `printf 'generated\n'`},
			},
		},
	}
	if err := manifest.SaveFile(manifest.Path(root), stack); err != nil {
		t.Fatalf("SaveFile(angee.yaml) error = %v", err)
	}
	t.Setenv("ANGEE_OPERATOR_URL", "")

	var stdout, stderr bytes.Buffer
	cmd := NewRoot(&stdout, &stderr)
	cmd.SetArgs([]string{"--root", root, "job", "run", "codegen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v; stderr = %q", err, stderr.String())
	}
	if got := stdout.String(); got != "generated\n" {
		t.Fatalf("stdout = %q, want one copy of %q", got, "generated\n")
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}
