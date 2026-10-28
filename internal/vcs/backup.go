package vcs

import (
	"context"
	"path/filepath"
	"slices"
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
	// Colocated is set in a BackupLayer's plan only once the layer has
	// verified its metadata against the declared store and found its commits
	// kept in the checkout's own Git store.
	Colocated *ColocatedMetadata
	Evidence  api.BackupSourceEvidence
}

// ColocatedMetadata is what a layer colocated over a Git checkout keeps in it
// of its own, which the layer's publication verdict covers in place of Git's:
// directories directly in the checkout's root (absolute paths) and Git ref
// namespaces, which the Git verdict disregards, and the commits the layer's
// verdict counts as published, which the Git verdict must find advertised.
// Git's reflogs, HEAD's included, still count.
type ColocatedMetadata struct {
	Dirs      []string
	GitRefs   []string
	Published []string
}

// BackupPlanner is the capability of a Driver whose checkouts a source capture
// can copy. The service asks the slot's driver for it; a source whose driver
// lacks it is refused.
type BackupPlanner interface {
	// BackupPlan checks the checkout's native metadata against Slot.Primary,
	// the declared store, without changing the repository, and plans what a
	// capture copies. layer, when not nil, plans the version control system
	// colocated over the checkout once the checkout's own store is verified,
	// and its plan is merged in (BackupPlan.Layered). With remoteOnly, the
	// layer's publication verdict and then the checkout's must pass; the
	// checkout's leaves exactly the layer's Colocated metadata to the layer's.
	BackupPlan(ctx context.Context, s Slot, remoteOnly bool, layer BackupLayer) (BackupPlan, error)
}

// BackupLayer plans the metadata of a version control system colocated over a
// Git checkout and keeping its commits in the checkout's Git store, as jj does.
type BackupLayer interface {
	// BackupLayerPlan checks the layer's metadata in s, if it has any, against
	// Slot.Primary and against gitStore, the Git store the caller verified for
	// s, and returns its components and evidence. With remoteOnly it refuses
	// visible history the layer holds that its remotes lack; history the
	// layer has hidden, such as jj's operation log, is its own metadata. A
	// checkout without the layer's metadata gets a plan without components.
	BackupLayerPlan(ctx context.Context, s Slot, gitStore string, remoteOnly bool) (BackupPlan, error)
}

// Layered returns p with layer's plan merged in: layer's components and
// worktree exclusions after p's, and layer's JJ evidence.
func (p BackupPlan) Layered(layer BackupPlan) BackupPlan {
	p.Components = append(slices.Clone(p.Components), layer.Components...)
	p.WorktreeExclude = append(slices.Clone(p.WorktreeExclude), layer.WorktreeExclude...)
	p.Evidence.JJRevision = layer.Evidence.JJRevision
	p.Evidence.JJChangeID = layer.Evidence.JJChangeID
	p.Evidence.JJOperation = layer.Evidence.JJOperation
	p.Evidence.JJWorkspaces = layer.Evidence.JJWorkspaces
	return p
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
