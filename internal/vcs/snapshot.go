package vcs

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ang-ee/angee-operator/api"
)

// SnapshotComponent is checked native repository metadata, copied without following links.
// Name identifies its portable recovery role, independently of its original location.
type SnapshotComponent struct {
	Name, Root string
	Exclude    []string
}

// SnapshotPlan describes immutable snapshot inputs. Drivers own metadata topology;
// the service owns filesystem confinement, selection, archive layout and IO.
type SnapshotPlan struct {
	RepositoryRoot  string
	Components      []SnapshotComponent
	WorktreeExclude []string
	// Colocated is set in a SnapshotLayer's plan only once the layer has
	// verified its metadata against the declared store and found its commits
	// kept in the checkout's own Git store.
	Colocated *ColocatedMetadata
	Evidence  api.SnapshotSourceEvidence
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

// SnapshotPlanner is the capability of a Driver whose checkouts a source snapshot
// can copy. The service asks the slot's driver for it; a source whose driver
// lacks it is refused.
type SnapshotPlanner interface {
	// SnapshotPlan checks the checkout's native metadata against Slot.Primary,
	// the declared store, without changing the repository, and plans what a
	// snapshot copies. layer, when not nil, plans the version control system
	// colocated over the checkout once the checkout's own store is verified,
	// and its plan is merged in (SnapshotPlan.Layered). With remoteOnly, the
	// layer's publication verdict and then the checkout's must pass; the
	// checkout's leaves exactly the layer's Colocated metadata to the layer's.
	SnapshotPlan(ctx context.Context, s Slot, remoteOnly bool, layer SnapshotLayer) (SnapshotPlan, error)
}

// SnapshotLayer plans the metadata of a version control system colocated over a
// Git checkout and keeping its commits in the checkout's Git store, as jj does.
type SnapshotLayer interface {
	// SnapshotLayerPlan checks the layer's metadata in s, if it has any, against
	// Slot.Primary and against gitStore, the Git store the caller verified for
	// s, and returns its components and evidence. With remoteOnly it refuses
	// visible history the layer holds that its remotes lack; history the
	// layer has hidden, such as jj's operation log, is its own metadata. A
	// checkout without the layer's metadata gets a plan without components.
	SnapshotLayerPlan(ctx context.Context, s Slot, gitStore string, remoteOnly bool) (SnapshotPlan, error)
}

// Layered returns p with layer's plan merged in: layer's components and
// worktree exclusions after p's, and layer's JJ evidence.
func (p SnapshotPlan) Layered(layer SnapshotPlan) SnapshotPlan {
	p.Components = append(slices.Clone(p.Components), layer.Components...)
	p.WorktreeExclude = append(slices.Clone(p.WorktreeExclude), layer.WorktreeExclude...)
	p.Evidence.JJRevision = layer.Evidence.JJRevision
	p.Evidence.JJChangeID = layer.Evidence.JJChangeID
	p.Evidence.JJOperation = layer.Evidence.JJOperation
	p.Evidence.JJWorkspaces = layer.Evidence.JJWorkspaces
	return p
}

// SnapshotCapabilityError carries a static public capability refusal without paths or secrets.
type SnapshotCapabilityError struct{ Reason string }

func (e *SnapshotCapabilityError) Error() string { return e.Reason }
func SnapshotRefusal(reason string) error        { return &SnapshotCapabilityError{Reason: reason} }

// SnapshotWithin checks lexical containment of driver-discovered metadata. The
// snapshot caller must additionally open components through a confined root.
func SnapshotWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
