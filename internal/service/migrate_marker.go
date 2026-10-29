package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/copierx"
)

// migrateMarker is what a migrate records under the stack root while the
// slot may be set aside, from just before it is renamed aside until the
// migrate finishes or is rolled back. A migrate killed, or whose rollback
// stopped, leaves it, so the slot's checkout, under a private name beside its
// path, is reported rather than lost: by status, repair, destroy and the next
// migrate of the slot (unfinishedMigrate).
type migrateMarker struct {
	Workspace string    `json:"workspace"`
	Slot      string    `json:"slot"`
	Path      string    `json:"path"`
	Primary   string    `json:"primary"`
	To        string    `json:"to"`
	Started   time.Time `json:"started"`
}

// migrateMarkerRel is where the marker of a migrate of slot of workspace
// lives, relative to the stack root.
func migrateMarkerRel(workspaceName, slot string) string {
	return filepath.ToSlash(filepath.Join("run", "migrate", url.PathEscape(workspaceName), url.PathEscape(slot)+".json"))
}

// migrateMarkerPath is the marker's absolute path.
func (p *Platform) migrateMarkerPath(workspaceName, slot string) string {
	return filepath.Join(p.root, filepath.FromSlash(migrateMarkerRel(workspaceName, slot)))
}

// beginMarker records the migrate, through a guard on the stack root.
func (m *migration) beginMarker() error {
	if m.marked {
		return nil
	}
	data, err := json.Marshal(migrateMarker{Workspace: m.workspaceName, Slot: m.slot, Path: m.path, Primary: m.primary, To: m.to, Started: time.Now().UTC()})
	if err != nil {
		return err
	}
	guard, err := copierx.OpenGuardedPath(m.p.root, m.p.root, migrateMarkerRel(m.workspaceName, m.slot), nil)
	if err != nil {
		return fmt.Errorf("record the migrate of source slot %q: %w", m.slot, err)
	}
	defer func() { _ = guard.Close() }()
	if err := guard.WriteFile(append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("record the migrate of source slot %q: %w", m.slot, err)
	}
	m.marked = true
	return nil
}

// endMarker removes the migrate's record, once the slot is migrated or
// rolled back.
func (m *migration) endMarker() error {
	if !m.marked {
		return nil
	}
	guard, err := copierx.OpenGuardedPath(m.p.root, m.p.root, migrateMarkerRel(m.workspaceName, m.slot), nil)
	if err == nil {
		err = guard.RemoveAll()
		_ = guard.Close()
	}
	if err != nil {
		return fmt.Errorf("the migrate's record %s was left (%v); delete it", m.p.migrateMarkerPath(m.workspaceName, m.slot), err)
	}
	m.marked = false
	return nil
}

// unfinishedMigrate reports a migrate of slot, checked out at path, that left
// its record behind, and where what was the slot's checkout may be: the
// entries with the migrate's private prefix beside path (asidesBeside). The
// reason ends with the command that points the slot's registration back once
// its checkout is moved back.
func (p *Platform) unfinishedMigrate(workspaceName, slot, path string) (string, bool) {
	marker := p.migrateMarkerPath(workspaceName, slot)
	data, err := os.ReadFile(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	reason := fmt.Sprintf("a migrate of source slot %q did not finish, as %s records", slot, marker)
	if asides := asidesBeside(path); len(asides) > 0 {
		reason += fmt.Sprintf("; what was its checkout may be set aside at %s", strings.Join(asides, ", "))
	}
	var record migrateMarker
	if err == nil && json.Unmarshal(data, &record) == nil && record.Primary != "" {
		reason += fmt.Sprintf("; once it is back at %s, point its git registration at it with `%s`", path, repairCommand(record.Primary, path))
	}
	return reason, true
}

// asidesBeside lists what a migrate set aside beside the slot path path: the
// entries of its parent with the migrate's private prefix. A migrate leaves
// one when it is killed, when its rollback stops, when the old checkout held
// what was written into it late, or when the new one could not be moved back.
func asidesBeside(path string) []string {
	var asides []string
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), asidePrefix) {
			asides = append(asides, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	return asides
}

// migrateIncomplete is the refusal of a verb on slot, at path, that a migrate
// did not finish (unfinishedMigrate, whose reason it is),
// PRECONDITION_FAILED, cause migrate_incomplete.
func (p *Platform) migrateIncomplete(workspaceName, slot, source, path, reason string) error {
	hint := fmt.Sprintf("Put the slot's checkout back at %s from where it was set aside, or remove what is left of it, then delete %s.", path, p.migrateMarkerPath(workspaceName, slot))
	return &OperationError{
		Code:      CodePreconditionFailed,
		Cause:     CauseMigrateIncomplete,
		Workspace: workspaceName,
		Slot:      slot,
		Source:    source,
		Paths:     []string{path},
		Hint:      hint,
		Summary:   fmt.Sprintf("%s. %s", reason, hint),
	}
}
