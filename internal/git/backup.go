package git

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// PublishedBackup verifies a clean checkout's exact revision against current
// remote advertisements. It does not fetch, move refs, or modify the repository.
// All local branch tips must also be published: a remote-only payload must not
// silently discard a private branch. The returned map contains remote names and
// URLs for trusted recovery tooling, never transport log fields.
func (c Client) PublishedBackup(ctx context.Context, dir string) (map[string]string, error) {
	status, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if len(status) > 0 {
		return nil, backupPublicationRefusal("remote-only capture refuses uncommitted or untracked source files")
	}
	ignored, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "ls-files", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	if len(ignored) > 0 {
		return nil, backupPublicationRefusal("remote-only capture refuses local ignored files; use portable capture")
	}
	refs, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	head, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, err
	}
	var revisions []string
	for _, line := range strings.Split(string(refs), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !strings.HasPrefix(fields[0], "refs/heads/") && !strings.HasPrefix(fields[0], "refs/tags/") && !strings.HasPrefix(fields[0], "refs/remotes/") {
			return nil, backupPublicationRefusal("remote-only capture refuses local recovery refs (stash, notes, replace or custom refs); choose portable capture")
		}
		revisions = append(revisions, fields[1])
	}
	revisions = append(revisions, strings.TrimSpace(string(head)))
	names, err := c.Remotes(ctx, dir)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	advertised := map[string]bool{}
	remotes := map[string]string{}
	for _, name := range names {
		url, ok, err := c.RemoteURL(ctx, dir, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		err = RunNetworkOperation(ctx, "verify backup publication", dir, func(ctx context.Context) error {
			out, _, runErr := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "ls-remote", EndOfOptions, name)
			if runErr != nil {
				return runErr
			}
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					advertised[fields[0]] = true
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		remotes[name] = url
	}
	for _, revision := range revisions {
		if !advertised[revision] {
			return nil, backupPublicationRefusal("remote-only capture refuses source revisions absent from current remote advertisements")
		}
	}
	// Reflogs can retain local-only commits even when every branch was reset.
	tips := make([]string, 0, len(advertised))
	for revision := range advertised {
		tips = append(tips, revision)
	}
	slices.Sort(tips)
	args := append([]string{"--no-pager", "rev-list", "--all", "--reflog", "--not", EndOfOptions}, tips...)
	private, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, args...)
	if err != nil {
		return nil, fmt.Errorf("cannot prove local recovery history published; choose portable capture: %w", err)
	}
	if len(private) > 0 {
		return nil, backupPublicationRefusal("remote-only capture refuses unpublished reflog history; choose portable capture")
	}
	return remotes, nil
}

// BackupPublicationError carries an actionable safe refusal without filesystem
// paths, transport URLs, or command output. The serving layer may display it.
type BackupPublicationError struct{ Reason string }

func (e *BackupPublicationError) Error() string    { return e.Reason }
func backupPublicationRefusal(reason string) error { return &BackupPublicationError{Reason: reason} }
