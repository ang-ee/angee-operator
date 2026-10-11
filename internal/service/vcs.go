package service

import (
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
)

// slotDriver returns the driver that reads slot. The git driver reads every
// slot until jj slots exist.
func (p *Platform) slotDriver(slot vcs.Slot) vcs.Driver {
	if p.slotDrivers != nil {
		return p.slotDrivers(slot)
	}
	return gitdriver.New(p.gitClient())
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

// sourceBaseRemote names the remote a source's base refs come from: its clone
// remote (origin for `repo:`), or "" when it declares none.
func sourceBaseRemote(source manifest.Source) string {
	name, _, _ := source.CloneRemote()
	return name
}
