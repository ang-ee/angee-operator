package service

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/fyltr/angee/api"
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
	return runDiffAt(ctx, p.sourcePath(name, source), ref)
}

// WorkspaceSourceDiff returns the unified-diff hunks for a per-workspace
// source slot. Semantics for ref mirror SourceDiff.
func (p *Platform) WorkspaceSourceDiff(ctx context.Context, workspace, slot, ref string) ([]api.DiffFile, error) {
	stack, _, _, path, err := p.workspaceSourceTarget(ctx, workspace, slot)
	if err != nil {
		return nil, err
	}
	_ = stack
	return runDiffAt(ctx, path, ref)
}

func runDiffAt(ctx context.Context, workdir, ref string) ([]api.DiffFile, error) {
	if workdir == "" {
		return nil, &NotFoundError{Kind: "worktree", Name: workdir}
	}
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if ref != "" {
		args = append(args, ref)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workdir
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s in %s: %w (%s)", strings.Join(args, " "), workdir, err, strings.TrimSpace(stderr.String()))
	}
	files, _, err := gitdiff.Parse(stdout)
	if err != nil {
		return nil, fmt.Errorf("parse diff: %w", err)
	}
	out := make([]api.DiffFile, 0, len(files))
	for _, f := range files {
		out = append(out, convertDiffFile(f))
	}
	return out, nil
}

func convertDiffFile(f *gitdiff.File) api.DiffFile {
	mode := ""
	switch {
	case f.NewMode != 0:
		mode = fmt.Sprintf("%o", f.NewMode)
	case f.OldMode != 0:
		mode = fmt.Sprintf("%o", f.OldMode)
	}
	hunks := make([]api.DiffHunk, 0, len(f.TextFragments))
	for _, frag := range f.TextFragments {
		hunks = append(hunks, convertFragment(frag))
	}
	return api.DiffFile{
		OldPath:   f.OldName,
		NewPath:   f.NewName,
		Mode:      mode,
		IsBinary:  f.IsBinary,
		IsNew:     f.IsNew,
		IsDeleted: f.IsDelete,
		IsRename:  f.IsRename,
		Hunks:     hunks,
	}
}

func convertFragment(frag *gitdiff.TextFragment) api.DiffHunk {
	body := &bytes.Buffer{}
	for _, line := range frag.Lines {
		body.WriteString(line.Op.String())
		body.WriteString(line.Line)
		if !strings.HasSuffix(line.Line, "\n") {
			body.WriteByte('\n')
		}
	}
	return api.DiffHunk{
		OldStart: int(frag.OldPosition),
		OldLines: int(frag.OldLines),
		NewStart: int(frag.NewPosition),
		NewLines: int(frag.NewLines),
		Header:   strings.TrimSpace(frag.Comment),
		Body:     body.String(),
	}
}
