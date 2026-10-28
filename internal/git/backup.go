package git

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
)

// publishedRefs are the ref namespaces a remote-only capture accepts: every
// ref in them must be advertised by a remote.
var publishedRefs = []string{"refs/heads/", "refs/tags/", "refs/remotes/"}

// Colocated is the metadata a version control system colocated over a Git
// checkout keeps in it, such as jj's, once that system has verified that it
// keeps its commits in the checkout's own Git store. That system's own
// verdict covers its visible history, so PublishedBackup disregards exactly
// this metadata: the ignored files below Dirs, directories directly in the
// checkout's root (".jj"), and the refs under Refs, private ref namespaces
// ("refs/jj/"). Reflogs still count, HEAD's included even where the colocated
// system wrote it, and so does a reflog of a ref under Refs: git's --exclude
// does not reach --reflog. Revisions are the commits the colocated verdict
// counts as published; each must be advertised too. The zero value
// disregards nothing.
type Colocated struct {
	Dirs      []string
	Refs      []string
	Revisions []string
}

// valid refuses metadata that would hide more than a colocated system's own:
// anything but a directory directly in the checkout's root, or a namespace
// overlapping publishedRefs.
func (c Colocated) valid() error {
	for _, dir := range c.Dirs {
		if dir == "" || dir == "." || dir == ".." || strings.Contains(dir, "/") {
			return fmt.Errorf("colocated metadata directory %q is not directly in the checkout's root", dir)
		}
	}
	for _, namespace := range c.Refs {
		overlaps := slices.ContainsFunc(publishedRefs, func(public string) bool {
			return strings.HasPrefix(namespace, public) || strings.HasPrefix(public, namespace)
		})
		if overlaps || !strings.HasPrefix(namespace, "refs/") || !strings.HasSuffix(namespace, "/") || strings.ContainsAny(namespace, "*?[\\") {
			return fmt.Errorf("colocated ref namespace %q is not private", namespace)
		}
	}
	return nil
}

// PublishedBackup verifies a clean checkout's exact revision against current
// remote advertisements. It does not fetch, move refs, or modify the repository.
// All local branch tips must also be published: a remote-only payload must not
// silently discard a private branch. The returned map contains remote names and
// URLs for trusted recovery tooling, never transport log fields. It leaves
// colocated's metadata to that system's verdict and verifies its revisions, as
// Colocated describes; anything else in the checkout counts.
func (c Client) PublishedBackup(ctx context.Context, dir string, colocated Colocated) (map[string]string, error) {
	if err := colocated.valid(); err != nil {
		return nil, err
	}
	status, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if len(status) > 0 {
		return nil, backupPublicationRefusal("remote-only capture refuses uncommitted or untracked source files")
	}
	ignored, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "--no-pager", "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for file := range bytes.SplitSeq(ignored, []byte{0}) {
		name := string(file)
		if name == "" || slices.ContainsFunc(colocated.Dirs, func(dir string) bool { return strings.HasPrefix(name, dir+"/") }) {
			continue
		}
		return nil, backupPublicationRefusal("remote-only capture refuses local ignored files; use the bundle source policy")
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
		within := func(namespace string) bool { return strings.HasPrefix(fields[0], namespace) }
		if slices.ContainsFunc(colocated.Refs, within) {
			continue
		}
		if !slices.ContainsFunc(publishedRefs, within) {
			return nil, backupPublicationRefusal("remote-only capture refuses local recovery refs (stash, notes, replace or custom refs); use the bundle source policy")
		}
		revisions = append(revisions, fields[1])
	}
	revisions = append(revisions, strings.TrimSpace(string(head)))
	revisions = append(revisions, colocated.Revisions...)
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
	args := []string{"--no-pager", "rev-list"}
	for _, namespace := range colocated.Refs {
		args = append(args, "--exclude="+namespace+"*")
	}
	args = append(append(args, "--all", "--reflog", "--not", EndOfOptions), tips...)
	private, _, err := c.RunIsolated(ctx, dir, []string{"GIT_OPTIONAL_LOCKS=0"}, args...)
	if err != nil {
		return nil, fmt.Errorf("cannot prove local recovery history published; use the bundle source policy: %w", err)
	}
	if len(private) > 0 {
		return nil, backupPublicationRefusal("remote-only capture refuses unpublished reflog history; use the bundle source policy")
	}
	return remotes, nil
}

// BackupPublicationError carries an actionable safe refusal without filesystem
// paths, transport URLs, or command output. The serving layer may display it.
type BackupPublicationError struct{ Reason string }

func (e *BackupPublicationError) Error() string    { return e.Reason }
func backupPublicationRefusal(reason string) error { return &BackupPublicationError{Reason: reason} }
