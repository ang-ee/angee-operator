// Package vcs is the seam between the service and the version control system
// behind a workspace slot. The service owns what the manifest says about a
// slot (its path, its base, the branch it must be on); a Driver owns how the
// slot's checkout answers for it. Git is the only driver today; jj slots add a
// second one.
package vcs

import (
	"context"

	"github.com/ang-ee/angee-operator/api"
)

// Slot identifies one slot checkout and the facts a driver needs about it.
type Slot struct {
	Path    string // the checkout
	Branch  string // the branch the checkout must be on; empty when any ref will do
	BaseRef string // the slot's ref, else its source's default ref; may be empty
}

// Status is what a driver reads from a slot's checkout.
type Status struct {
	CurrentRef string
	Upstream   string // empty without an upstream
	Dirty      bool
	// Ahead and Behind count against Upstream when it is set, else against
	// BaseRef, or BaseRef's remote counterpart when there is no local one.
	Ahead, Behind int
	// MismatchReason is set when the checkout is not on Slot.Branch.
	MismatchReason string
	// Pushed reports that nothing in the checkout is held by it alone, so
	// destroying it loses no work. UnpushedReason says what is held when it is
	// not.
	Pushed         bool
	UnpushedReason string
}

// Driver reads one kind of slot checkout.
type Driver interface {
	// Status reads the checkout. On error, the returned Status holds what was
	// read before the failure; with a *CountError, that includes Pushed and
	// UnpushedReason.
	Status(ctx context.Context, s Slot) (Status, error)
	// Diff returns the checkout's uncommitted changes when ref is empty, else
	// the committed range from the checkout to ref.
	Diff(ctx context.Context, s Slot, ref string) ([]api.DiffFile, error)
}

// CountError reports that Status decided whether a checkout is pushed but
// could not count it against its upstream or base, for example because the
// base branch was deleted. The Status returned with it has a valid Pushed and
// UnpushedReason; its Ahead and Behind are not set.
type CountError struct {
	Err error
}

func (e *CountError) Error() string { return e.Err.Error() }

func (e *CountError) Unwrap() error { return e.Err }
