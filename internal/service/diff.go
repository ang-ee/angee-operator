package service

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/ang-ee/angee-operator/internal/vcs/gitdriver"
)

// SourceDiff returns the unified-diff hunks for a top-level source. With
// ref empty, the diff is "working tree vs HEAD" (uncommitted changes).
// With ref non-empty, the diff is "HEAD..ref" (committed range).
func (p *Platform) SourceDiff(ctx context.Context, name, ref string) ([]api.DiffFile, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	source, ok := stack.Sources[name]
	if !ok {
		return nil, &NotFoundError{Kind: "source", Name: name}
	}
	if source.Kind != "git" {
		return nil, &InvalidInputError{Field: "name", Reason: fmt.Sprintf("source %q is %s, only git sources can be diffed", name, source.Kind)}
	}
	return gitdriver.New(p.gitClient()).Diff(ctx, vcs.Slot{Path: p.sourcePath(name, source)}, ref)
}

// WorkspaceSourceDiff returns the unified-diff hunks for a per-workspace
// source slot. Semantics for ref mirror SourceDiff.
func (p *Platform) WorkspaceSourceDiff(ctx context.Context, workspace, slot, ref string) ([]api.DiffFile, error) {
	_, wsSource, source, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return nil, err
	}
	target := workspaceVCSSlot(path, source, wsSource)
	return p.slotDriver(target).Diff(ctx, target, ref)
}
