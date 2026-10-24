package vcs

import (
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
)

// BackupComponent is checked native repository metadata, copied without following links.
// Name identifies its portable recovery role, independently of its original location.
type BackupComponent struct {
	Name, Root string
	Exclude    []string
}

// BackupPlan describes immutable capture inputs. Drivers own metadata topology;
// the service owns filesystem confinement, selection, archive layout and IO.
type BackupPlan struct {
	RepositoryRoot  string
	Components      []BackupComponent
	WorktreeExclude []string
	Evidence        api.BackupSourceEvidence
}

// BackupCapabilityError carries a static public capability refusal without paths or secrets.
type BackupCapabilityError struct{ Reason string }

func (e *BackupCapabilityError) Error() string { return e.Reason }
func BackupRefusal(reason string) error        { return &BackupCapabilityError{Reason: reason} }

// BackupWithin checks lexical containment of driver-discovered metadata. The
// capture caller must additionally open components through a confined root.
func BackupWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
