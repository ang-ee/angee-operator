package vcs

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// DiffFiles converts a parsed git-format diff into the API's shape. git and
// jj (`jj diff --git`) both print that format, so every driver's Diff ends
// here.
func DiffFiles(files []*gitdiff.File) []api.DiffFile {
	out := make([]api.DiffFile, 0, len(files))
	for _, f := range files {
		out = append(out, convertDiffFile(f))
	}
	return out
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
