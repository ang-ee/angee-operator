package proccompose

import (
	"os"
	"os/exec"
	"testing"
)

func requireProcessComposeIntegration(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("process-compose"); err != nil {
		if os.Getenv("ANGEE_REQUIRE_PROCESS_COMPOSE") == "1" {
			t.Fatalf("ANGEE_REQUIRE_PROCESS_COMPOSE=1 but process-compose is unavailable: %v", err)
		}
		t.Skip("process-compose unavailable")
	}
}
