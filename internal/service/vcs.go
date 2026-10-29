package service

import (
	"github.com/ang-ee/angee-operator/internal/copierx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
	"github.com/ang-ee/angee-operator/internal/vcs/jjdriver"
)

// A slot's driver writes its path through the guard the service opened.
var _ vcs.Dest = (*copierx.GuardedPath)(nil)

// slotDriver returns the driver for slot. The git driver serves every slot
// until jj slots exist.
func (p *Platform) slotDriver(slot vcs.Slot) vcs.Driver {
	if p.slotDrivers != nil {
		return p.slotDrivers(slot)
	}
	return gitdriver.New(p.gitClient())
}

// backupLayer returns the planner a source capture hands slot's driver for
// the version control system colocated over its Git checkout: jj, which plans
// nothing for a checkout without jj metadata, so the layer is found on disk
// rather than named by the manifest. With slotDriver it is the one place a
// capture picks its drivers; phase 6 of .agents/plans/jj-native-operator.md
// picks them here by the slot's mode.
func (p *Platform) backupLayer(slot vcs.Slot) vcs.BackupLayer {
	if p.backupLayers != nil {
		return p.backupLayers(slot)
	}
	return jjdriver.New(p.gitClient())
}

// workspaceSlotDriver returns the driver for the workspace slot checked out at
// path, with the slot it is to be handed.
func (p *Platform) workspaceSlotDriver(path string, source manifest.Source, wsSource manifest.WorkspaceSource) (vcs.Driver, vcs.Slot) {
	slot := workspaceVCSSlot(path, source, wsSource)
	return p.slotDriver(slot), slot
}

// workspaceVCSSlot describes the workspace slot checked out at path to its
// driver. Only a worktree slot that names a branch must be on it
// (workspaceSourceRequiresBranch).
func workspaceVCSSlot(path string, source manifest.Source, wsSource manifest.WorkspaceSource) vcs.Slot {
	slot := vcs.Slot{Path: path, BaseRef: workspaceSourceBaseRef(source, wsSource), BaseRemote: sourceBaseRemote(source)}
	if workspaceSourceRequiresBranch(wsSource) {
		slot.Branch = wsSource.Branch
	}
	return slot
}

// workspaceLifecycleSlot describes the git slot at path to its driver for
// creating or removing it: workspaceVCSSlot's facts, and how the checkout is
// made. A slot whose manifest mode is worktree is a worktree of its source's
// cache; any other git slot, an omitted mode included, is a clone from the
// source's clone remote.
func (p *Platform) workspaceLifecycleSlot(path string, source manifest.Source, wsSource manifest.WorkspaceSource) vcs.Slot {
	slot := workspaceVCSSlot(path, source, wsSource)
	if wsSource.Mode == manifest.WorkspaceSourceModeWorktree {
		slot.Mode = vcs.ModeWorktree
		slot.Primary = p.sourcePath(wsSource.Source, source)
	} else {
		slot.Mode = vcs.ModeClone
		slot.CloneURL = source.CloneURL()
	}
	return slot
}

// sourceBaseRemote names the remote a source's base refs come from: its clone
// remote (origin for `repo:`), or "" when it declares none.
func sourceBaseRemote(source manifest.Source) string {
	name, _, _ := source.CloneRemote()
	return name
}
