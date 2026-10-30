package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// CompleteCheckout clones all history and tags, resolves revision once, then
// checks out that immutable commit and its pinned submodules. It does not share
// objects with a local repository, use a shallow clone, or run submodule update
// commands configured by the repository. Native Git retains credential helpers,
// SSH configuration and relative submodule URL semantics.
func (c Client) CompleteCheckout(ctx context.Context, repository, dest, revision string) (string, error) {
	// Prefixing the candidate keeps option-shaped input out of option parsing;
	// Git itself validates its revision/ref vocabulary, excluding refspecs.
	if _, err := c.Run(ctx, "", "check-ref-format", "refs/"+revision); err != nil {
		return "", err
	}
	if err := c.Clone(ctx, repository, dest, "--no-checkout", "--no-local", "--origin", "origin", "--tags"); err != nil {
		return "", err
	}
	// Fetching the requested name also supports non-default branches, full
	// refs and immutable commit IDs without resolving a moving name twice.
	if _, err := c.runNetwork(ctx, dest, dest, "git fetch checkout revision", "fetch", "--tags", "--no-recurse-submodules", EndOfOptions, "origin", revision); err != nil {
		return "", err
	}
	commit, err := c.Commit(ctx, dest, "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	shallow, err := c.runText(ctx, dest, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", err
	}
	if shallow != "false" {
		return "", fmt.Errorf("complete checkout requires a repository with full history")
	}
	// A user's global sparse-checkout setting must not omit package inputs.
	if _, err := c.Run(ctx, dest, "config", "core.sparseCheckout", "false"); err != nil {
		return "", err
	}
	if err := c.CheckoutDetached(ctx, dest, commit); err != nil {
		return "", err
	}
	if err := c.completeSubmodules(ctx, dest); err != nil {
		return "", err
	}
	actual, err := c.Commit(ctx, dest, "HEAD")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(actual) != commit {
		return "", fmt.Errorf("checkout revision changed while preparing submodules")
	}
	return commit, nil
}

// completeSubmodules supplies explicit paths at each level: Git's --recursive
// helper otherwise honours submodule.active and may silently omit package inputs.
// The native index owns the gitlinks; Git owns initialization, URL resolution and
// checkout of each pinned commit. No .gitmodules parser or config copy is needed.
func (c Client) completeSubmodules(ctx context.Context, dir string) error {
	paths, err := c.Submodules(ctx, dir)
	if err != nil || len(paths) == 0 {
		return err
	}
	args := []string{"-c", "core.sparseCheckout=false", "submodule", "update", "--init", "--checkout", "--no-recommend-shallow", "--"}
	args = append(args, paths...)
	if _, err := c.runNetwork(ctx, dir, dir, "git submodule update", args...); err != nil {
		return err
	}
	for _, path := range paths {
		child := filepath.Join(dir, path)
		shallow, err := c.runText(ctx, child, "rev-parse", "--is-shallow-repository")
		if err != nil {
			return err
		}
		if shallow != "false" {
			return fmt.Errorf("complete checkout requires submodules with full history")
		}
		if err := c.completeSubmodules(ctx, child); err != nil {
			return err
		}
	}
	return nil
}
