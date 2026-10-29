//go:build windows

package proccompose

import (
	"context"
	"errors"
	"testing"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

func TestTransientJobRefusesWindowsBeforeSpawn(t *testing.T) {
	backend := NewBackend()
	_, err := backend.runTransientJob(context.Background(), runtime.Target{Root: t.TempDir()}, "must-not-run", Process{Command: "exit 0"})
	var unsupported *runtime.JobCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected native capability refusal before spawn: %v", err)
	}
	if err := backend.ValidateJob(runtime.JobSpec{}); err != nil {
		t.Fatalf("ordinary supervisor job refused: %v", err)
	}
}
