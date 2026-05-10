package stackroot

import (
	"os"
	"path/filepath"
)

// Resolve walks upward from start and returns the Angee control root.
//
// A manifest in the current directory wins first, followed by a manifest in a
// child .angee directory. Template-only project roots are also accepted: when a
// directory contains templates/workspaces or .templates/workspaces, Resolve
// returns that directory's .angee control path even if it has not been created
// yet. If no root marker is found, Resolve preserves the historical CLI
// behavior and returns the original start value.
func Resolve(start string) (string, error) {
	if start == "" {
		start = "."
	}
	absStart, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir := absStart
	for {
		if _, err := os.Stat(filepath.Join(dir, "angee.yaml")); err == nil {
			return dir, nil
		}
		control := filepath.Join(dir, ".angee")
		if _, err := os.Stat(filepath.Join(control, "angee.yaml")); err == nil {
			return control, nil
		}
		if hasWorkspaceTemplates(dir) {
			return control, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return start, nil
}

func hasWorkspaceTemplates(dir string) bool {
	for _, rel := range []string{
		filepath.Join(".templates", "workspaces"),
		filepath.Join("templates", "workspaces"),
	} {
		if info, err := os.Stat(filepath.Join(dir, rel)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}
