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

// The phases a migrate's record names.
const (
	// migratePhaseSettingAside: the slot is about to be set aside, or was a
	// moment ago, and nothing else is done.
	migratePhaseSettingAside = "setting-aside"
	// migratePhaseMigrating: the slot is set aside, and the record's steps
	// undo, in order, what is done since: entries carried into the new
	// checkout go back first, the new checkout is removed, then the slot's
	// checkout is moved back.
	migratePhaseMigrating = "migrating"
	// migratePhaseRollbackStopped: a rollback stopped, and the steps are what
	// it left undone.
	migratePhaseRollbackStopped = "rollback-stopped"
	// migratePhaseSaving: the manifest is being written: the steps undo the
	// migrate, unless the manifest records the new mode, when only the
	// clean-up (Cleanup) is left.
	migratePhaseSaving = "saving"
	// migratePhaseSaved: the manifest records the new mode; the steps are the
	// clean-up of the old checkout.
	migratePhaseSaved = "saved"
	// migratePhaseForgetting: the slot is migrated, and the new worktree is
	// set aside for the moment of forgetting the jj workspace it was.
	migratePhaseForgetting = "forgetting"
)

// migrateMarker is what a migrate records under the stack root while the
// slot may be set aside, from just before it is renamed aside until the
// migrate finishes or is rolled back: how far it got (Phase), and the steps
// that recover from there by hand, in order (Steps). Each step the migrate
// takes rewrites it. A migrate killed, or whose rollback stopped, leaves it,
// so the slot's checkout, under a private name beside its path, is reported
// with what to do rather than lost: by status, repair, destroy, the slot
// verbs and the next migrate of the slot (unfinishedMigrate).
type migrateMarker struct {
	Workspace string    `json:"workspace"`
	Slot      string    `json:"slot"`
	Path      string    `json:"path"`
	Primary   string    `json:"primary"`
	To        string    `json:"to"`
	Started   time.Time `json:"started"`
	Phase     string    `json:"phase"`
	Steps     []string  `json:"steps,omitempty"`
	// Cleanup, while the manifest is being written, is what is left once it
	// records the new mode.
	Cleanup []string `json:"cleanup,omitempty"`
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

// beginMarker records the migrate, before the slot is set aside.
func (m *migration) beginMarker() error {
	if m.marked {
		return nil
	}
	m.started = time.Now().UTC()
	if err := m.record(migratePhaseSettingAside, []string{fmt.Sprintf("if nothing is at %[1]s and a %[2]s… entry beside it (not one ending in %[3]s) is the slot's checkout, move it back: `%[4]s`", shellQuote(m.path), asidePrefix, keptSuffix, vacantCommand(m.path, fmt.Sprintf("mv <that entry> %s && %s", shellQuote(m.path), repairCommand(m.primary, m.path))))}); err != nil {
		return err
	}
	m.marked = true
	return nil
}

// record rewrites the migrate's record with phase and steps, through a guard
// on the stack root (an atomic replace).
func (m *migration) record(phase string, steps []string) error {
	return m.write(migrateMarker{Phase: phase, Steps: steps})
}

// recordSaving rewrites the record before the manifest is written: the undo
// steps, and the clean-up for a manifest that records the new mode.
func (m *migration) recordSaving() error {
	return m.write(migrateMarker{Phase: migratePhaseSaving, Steps: m.undoSteps(), Cleanup: m.cleanupSteps()})
}

// write writes record, filled with the migrate's own facts.
func (m *migration) write(record migrateMarker) error {
	record.Workspace, record.Slot, record.Path, record.Primary, record.To, record.Started = m.workspaceName, m.slot, m.path, m.primary, m.to, m.started
	data, err := json.MarshalIndent(record, "", "  ")
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
	return nil
}

// recordCleanup rewrites the record once the manifest is written, with the
// clean-up left, a warning when it cannot.
func (m *migration) recordCleanup(phase string, steps []string) {
	if !m.marked {
		return
	}
	if err := m.record(phase, steps); err != nil {
		m.warnings = append(m.warnings, err.Error())
	}
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

// recoverySteps words steps, the record's, as a hint: run them in order,
// then delete the record.
func recoverySteps(steps []string, marker string) string {
	if len(steps) == 0 {
		return fmt.Sprintf("Delete %s once the slot is as it should be.", marker)
	}
	numbered := make([]string, len(steps))
	for i, step := range steps {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, step)
	}
	return fmt.Sprintf("Recover by hand, in this order: %s; then delete %s.", strings.Join(numbered, "; "), marker)
}

// unfinishedMigrate reports a migrate of slot, checked out at path, that left
// its record behind: why (reason), and how to recover by hand from where it
// stopped (hint), the steps the record holds, so the advice fits how far it
// got: before the new checkout exists, the slot's checkout is moved back;
// after it, the entries carried into it go back first and it is removed;
// once the manifest records the new mode, only the old checkout is left to
// clean up. What a migrate set aside beside path (asidesBeside) is named
// too. A record that cannot be read gets the steps of the earliest phase.
func (p *Platform) unfinishedMigrate(workspaceName, slot, path string) (reason, hint string, unfinished bool) {
	marker := p.migrateMarkerPath(workspaceName, slot)
	data, err := os.ReadFile(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false
	}
	var record migrateMarker
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	reason = fmt.Sprintf("a migrate of source slot %q did not finish", slot)
	hint = recoverySteps(record.Steps, marker)
	switch {
	case err != nil:
		reason += fmt.Sprintf(" (its record %s cannot be read: %v)", marker, err)
	case record.Phase == migratePhaseSaved || record.Phase == migratePhaseForgetting:
		reason = fmt.Sprintf("a migrate of source slot %q to %s did not finish cleaning up: the manifest records the new mode, as %s records", slot, record.To, marker)
	case record.Phase == migratePhaseSaving:
		reason += fmt.Sprintf(" (%s), as %s records: it stopped while writing the manifest", record.Phase, marker)
		hint = fmt.Sprintf("If angee.yaml records mode %q for source slot %q, the migrate is done: %s Otherwise: %s", record.To, slot, recoverySteps(record.Cleanup, marker), recoverySteps(record.Steps, marker))
	default:
		reason += fmt.Sprintf(" (%s), as %s records", record.Phase, marker)
	}
	if asides := asidesBeside(path); len(asides) > 0 {
		reason += fmt.Sprintf("; set aside beside it: %s", strings.Join(asides, ", "))
	}
	return reason, hint, true
}

// asidesBeside lists what a migrate set aside beside the slot path path: the
// entries of its parent with the migrate's private prefix. A migrate leaves
// one when it is killed, when its rollback stops, when the old checkout held
// what was written into it late, or when the new one could not be moved back;
// an undo of it leaves a kept directory (keptSuffix) when the old checkout
// held a carried entry again.
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
// did not finish (unfinishedMigrate's reason and hint), PRECONDITION_FAILED,
// cause migrate_incomplete.
func (p *Platform) migrateIncomplete(workspaceName, slot, source, path, reason, hint string) error {
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
