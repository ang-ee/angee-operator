package service

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

// composeProjectName derives the Docker Compose project name for a stack.
//
// It MUST be unique per stack *instance* (root): the Compose project is a
// global namespace in the shared Docker daemon — it keys container, network,
// and volume names — so two stacks that share a name have their containers,
// networks, and volumes merged into one project even when they live in
// different roots and are managed by different operator daemons.
//
// stack.Name is a human-facing label and is not unique (every dev workspace
// defaults to the same example name), and a single-root operator cannot see
// other stacks to detect a clash. Uniqueness must therefore be constructed
// locally from the one identifier this operator knows is unique: its absolute
// root. The friendly base is kept as a readable prefix so containers still read
// like "notes-angee-1a2b3c4d-edge-1".
func composeProjectName(name, root string) string {
	base := sanitizeProjectName(name)
	if base == "" {
		base = "angee"
	}
	// The project name prefixes container/network/volume names, which carry
	// practical limits (~63-char DNS labels for networks). Bound the readable
	// base so a pathologically long manifest name can't produce names Docker
	// rejects; the per-root hash suffix still guarantees uniqueness. The cut
	// can't strand a leading separator (sanitizeProjectName already guarantees a
	// leading alphanumeric), but re-trim in case it lands on a trailing one.
	const maxBase = 30
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-_")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(abs))
	return fmt.Sprintf("%s-%x", base, sum[:4]) // 8 hex chars, stable per root
}

// sanitizeProjectName reduces an arbitrary stack label to a Docker Compose
// project-name fragment. Compose requires the project name to match
// ^[a-z0-9][a-z0-9_-]*$, so this lower-cases, replaces every other rune with
// '-' (collapsing runs), and trims separators from both ends to guarantee a
// leading alphanumeric. The result may be empty (e.g. a name of only illegal
// runes); callers supply a fallback. The hash suffix appended by
// composeProjectName is already [0-9a-f], so the combined name is always valid.
func sanitizeProjectName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-_")
}
