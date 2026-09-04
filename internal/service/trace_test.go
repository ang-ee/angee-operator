package service

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

func TestRunLocalCommandTracesRedactedArgsAndEnvironmentKeys(t *testing.T) {
	var logs bytes.Buffer
	ctx := logctx.With(t.Context(), slog.New(logctx.NewCLIHandler(&logs, slog.LevelDebug)))
	command := []string{"sh", "-c", "exit 0", "--token", "secret", "https://user:password@example.com/repo"}
	_, err := runLocalCommand(ctx, "", command, map[string]string{"API_TOKEN": "env-secret"}, nil)
	if err != nil {
		t.Fatalf("runLocalCommand() error = %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "exec sh -c exit 0 --token *** https://***@example.com/repo") ||
		!strings.Contains(got, "env=[API_TOKEN]") ||
		!strings.Contains(got, "exec finished duration=") {
		t.Fatalf("trace output = %q", got)
	}
	for _, secret := range []string{"secret", "env-secret", "password", "user"} {
		if strings.Contains(got, secret) {
			t.Fatalf("trace output leaked %q: %q", secret, got)
		}
	}
}
