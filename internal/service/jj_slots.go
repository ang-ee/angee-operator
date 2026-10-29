package service

import (
	"fmt"
	"strings"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// errJJSlotNotYet is what a verb phase 8 of the jj plan has not implemented
// on a jj slot wraps. It matches vcs.ErrUnsupported, as a verb a slot's
// driver does not support does.
var errJJSlotNotYet = fmt.Errorf("not supported on a jj slot yet: %w", vcs.ErrUnsupported)

// jjSlotNotYet refuses verb on the jj slots of workspace named in slots, with
// their paths, before any of them is touched. Phase 8 of the jj plan
// replaces these refusals verb by verb. Like a verb a slot's driver does not
// support, it is the caller's input at fault (INVALID_INPUT); its cause tells
// a client that the slot is a jj slot. Every verb on a slot asks this before
// it runs anything, so a jj slot never gets a git command, and an aggregate
// verb never acts on some slots and skips its jj slots.
func jjSlotNotYet(verb, workspace string, slots, paths []string) *OperationError {
	hint := "Run jj in the slot instead."
	quoted := make([]string, len(slots))
	for i, slot := range slots {
		quoted[i] = fmt.Sprintf("%q", slot)
	}
	noun := "jj slot"
	if len(slots) > 1 {
		noun = "jj slots"
	}
	opErr := &OperationError{
		Code:      CodeInvalidInput,
		Cause:     CauseJJSlotUnsupported,
		Workspace: workspace,
		Paths:     paths,
		Hint:      hint,
		Summary:   fmt.Sprintf("%s is not supported on %s %s yet. %s", verb, noun, strings.Join(quoted, ", "), hint),
		Err:       errJJSlotNotYet,
	}
	if len(slots) == 1 {
		opErr.Slot = slots[0]
	}
	return opErr
}

// refuseJJSlots refuses verb, an aggregate over every slot of workspace, when
// any of its slots is a jj slot: such a verb would otherwise skip it, or run
// git in it.
func (p *Platform) refuseJJSlots(verb, workspaceName string, workspace manifest.Workspace, stack *manifest.Stack) error {
	var slots, paths []string
	for _, slot := range sortedKeys(workspace.Sources) {
		wsSource := workspace.Sources[slot]
		if stack.EffectiveSourceMode(wsSource) != manifest.WorkspaceSourceModeJJ {
			continue
		}
		_, path, _ := p.workspaceSourcePath(workspaceName, slot, wsSource)
		slots = append(slots, slot)
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return jjSlotNotYet(verb, workspaceName, slots, paths)
}
