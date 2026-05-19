package agentruntime

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fyltr/angee/internal/copierx"
)

// TestAgentRuntimeTemplateMetadata is the single canary that the bundled
// agent-runtime template parses cleanly and continues to advertise the
// documented env-var contract (AGENT_ID required, MCP_URL/MCP_TOKEN
// optional). It runs as a unit test inside the repo so contract drift
// surfaces in CI before it ships to consumers.
func TestAgentRuntimeTemplateMetadata(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	templateDir := filepath.Dir(file)

	metadata, err := copierx.ReadMetadata(templateDir)
	if err != nil {
		t.Fatalf("ReadMetadata() error = %v", err)
	}
	if metadata.Kind != "workspace" {
		t.Fatalf("metadata.Kind = %q, want workspace", metadata.Kind)
	}
	if metadata.Name != "agent-runtime" {
		t.Fatalf("metadata.Name = %q, want agent-runtime", metadata.Name)
	}
	agentID, ok := metadata.Inputs["AGENT_ID"]
	if !ok {
		t.Fatalf("metadata.Inputs missing AGENT_ID: %+v", metadata.Inputs)
	}
	if !agentID.Required {
		t.Fatalf("AGENT_ID.Required = false, want true")
	}
	for _, optional := range []string{"MCP_URL", "MCP_TOKEN"} {
		def, ok := metadata.Inputs[optional]
		if !ok {
			t.Fatalf("metadata.Inputs missing %s: %+v", optional, metadata.Inputs)
		}
		if def.Required {
			t.Fatalf("%s.Required = true, want false", optional)
		}
	}
}
