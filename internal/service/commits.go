package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// sourceCommits walks the source's local repository starting from HEAD and
// returns up to `limit` commits in committer-time order (newest first).
// Each entry carries parents, ref names that point at the commit, the
// author summary, and an RFC3339Nano timestamp.
//
// Returns nil with no error when the repository can't be opened (e.g. the
// source hasn't materialised yet) — the caller is expected to surface the
// underlying error elsewhere (typically via SourceState.State="missing").
func (p *Platform) sourceCommits(ctx context.Context, repoPath string, limit int) ([]api.CommitRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Defense-in-depth: GitOpsTopologyWithCommits clamps the caller's
	// withCommits to gitOpsTopologyCommitsMax before reaching here.
	// Repeat the cap at the allocation boundary so the bound holds
	// even if a future caller forgets.
	if limit > gitOpsTopologyCommitsMax {
		limit = gitOpsTopologyCommitsMax
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return nil, nil
	}
	refIndex, err := buildRefIndex(repo)
	if err != nil {
		return nil, fmt.Errorf("build ref index for %s: %w", repoPath, err)
	}
	head, err := repo.Head()
	if err != nil {
		return nil, nil
	}
	walker, err := repo.Log(&gogit.LogOptions{From: head.Hash(), Order: gogit.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("git log on %s: %w", repoPath, err)
	}
	defer walker.Close()
	// Allocate the slice empty rather than with cap=limit. Even though
	// `limit` is clamped to gitOpsTopologyCommitsMax above, CodeQL's
	// `go/uncontrolled-allocation-size` query traces user input from
	// the HTTP layer down to the make() call and the local clamp isn't
	// recognised as a sanitizer. `append` grows the slice as we walk;
	// the loop body still respects the `count >= limit` break so the
	// total allocation is bounded the same way.
	var commits []api.CommitRef
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return commits, err
		}
		c, err := walker.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "operator: commit walk on %s truncated: %v\n", repoPath, err)
			}
			break
		}
		sha := c.Hash.String()
		parents := make([]string, 0, len(c.ParentHashes))
		for _, ph := range c.ParentHashes {
			parents = append(parents, ph.String())
		}
		commits = append(commits, api.CommitRef{
			SHA:     sha,
			Parents: parents,
			Refs:    refIndex[sha],
			Time:    c.Committer.When.UTC().Format(time.RFC3339Nano),
			Summary: firstLine(c.Message),
			Author:  fmt.Sprintf("%s <%s>", c.Author.Name, c.Author.Email),
		})
		count++
		if count >= limit {
			break
		}
	}
	return commits, nil
}

func buildRefIndex(repo *gogit.Repository) (map[string][]string, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	index := map[string][]string{}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		name := string(ref.Name())
		// Filter out the noisy HEAD pointer; concrete refs are what UIs care about.
		if name == "HEAD" {
			return nil
		}
		index[ref.Hash().String()] = append(index[ref.Hash().String()], name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for sha := range index {
		sort.Strings(index[sha])
	}
	return index, nil
}

func firstLine(message string) string {
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		return strings.TrimSpace(message[:i])
	}
	return strings.TrimSpace(message)
}
