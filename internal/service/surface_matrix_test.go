package service

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSurfaceMatrixMentionsEveryExportedPlatformMethod(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	data, err := os.ReadFile(filepath.Join(repoRoot, "docs", "SURFACES.md"))
	if err != nil {
		t.Fatalf("ReadFile(docs/SURFACES.md) error = %v", err)
	}
	doc := string(data)

	platformType := reflect.TypeOf((*Platform)(nil))
	for i := 0; i < platformType.NumMethod(); i++ {
		name := platformType.Method(i).Name
		if !strings.Contains(doc, "| `"+name+"` |") {
			t.Fatalf("docs/SURFACES.md does not classify Platform.%s", name)
		}
	}
}
