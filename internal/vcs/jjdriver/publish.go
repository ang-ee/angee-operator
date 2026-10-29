package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/vcs"
)

// A jj slot is on no branch: its commits are published under a bookmark X
// that publish sets on the commit under the working copy, `@-`, and pushes
// to a remote R (plan phase 8, steps 3 and 4). R is git's push remote for X
// in the slot's colocated git config, as git slots resolve theirs (Q8), and
// is always named to jj. Status counts the slot's upstream on the same R, so
// once published the slot reads as pushed.

// PushURL is the URL of remote, or, when remote is empty, of the remote a
// push of the slot's branch goes to (pushRemote); "" when it cannot be
// resolved.
func (d Driver) PushURL(ctx context.Context, s vcs.Slot, remote string) string {
	remote, err := d.pushRemote(ctx, s.Path, remote, s.Branch)
	if err != nil {
		return ""
	}
	url, ok, err := d.remoteURL(ctx, s.Path, remote)
	if err != nil || !ok {
		return ""
	}
	return url
}

// pushRemote is the remote a push of branch from the slot at dir goes to:
// remote when the caller names one, else the one git pushes branch to from
// the slot (git.Client.PushRemoteFor: the branch's pushRemote, then
// remote.pushDefault, which the cache takes from the source's push_default,
// the branch's remote, origin, the only remote). A jj git.push setting plays
// no part. When git names none, or the slot is not colocated with git and so
// has no git config to ask (git run there would read whatever repository
// encloses it), that is a *vcs.NoPushRemoteError saying so.
func (d Driver) pushRemote(ctx context.Context, dir, remote, branch string) (string, error) {
	if remote != "" {
		return remote, nil
	}
	if ok, err := colocated(dir); err != nil {
		return "", err
	} else if !ok {
		return "", &vcs.NoPushRemoteError{Reason: "the jj slot is not colocated with git, so it has no git config naming one"}
	}
	remote, err := d.git.PushRemoteFor(ctx, dir, branch)
	var none *git.NoPushRemoteError
	if errors.As(err, &none) {
		return "", &vcs.NoPushRemoteError{Reason: none.Reason}
	}
	if err != nil {
		return "", fmt.Errorf("resolve the push remote of %q in %s: %w", branch, dir, err)
	}
	return remote, nil
}

// remoteURL returns the URL of remote as the slot's repository has it, and
// whether it has that remote: from git's config when the slot is colocated,
// else from `jj git remote list`. jj's git pseudo-remote, which mirrors the
// colocated repository's own branches, is no remote to push to.
func (d Driver) remoteURL(ctx context.Context, dir, remote string) (string, bool, error) {
	if remote == gitRemote {
		return "", false, nil
	}
	if ok, err := colocated(dir); err != nil {
		return "", false, err
	} else if ok {
		return d.git.RemoteURL(ctx, dir, remote)
	}
	out, err := d.jj.store(ctx, dir, "git", "remote", "list")
	if err != nil {
		return "", false, err
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if name, url, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == remote {
			return url, true, nil
		}
	}
	return "", false, nil
}

// pending is what a push of the slot's branch to a remote would send, read
// at one snapshot (op).
type pending struct {
	op string
	// tip is the commit under the slot's working copy, `@-`: the one the
	// branch is set on.
	tip string
	// from is what the commits are counted from: the branch on the remote
	// when it is there (upstream), else the slot's base.
	from     target
	upstream bool
	// local reports that the repository has the branch as a local bookmark.
	local   bool
	commits []commitEntry
}

// nothing says why a push with no commits has nothing to send.
func (p pending) nothing() string {
	if p.upstream {
		return fmt.Sprintf("%s already holds every commit of the slot", p.from.name)
	}
	return fmt.Sprintf("no commits beyond %s", p.from.name)
}

// pendingPush snapshots the slot and reads what a push of branch to remote
// would send: the commits of `X@R..@-` when the remote has branch X, else of
// `B..@-`, B the slot's base as Status resolves it (a base that does not
// resolve wraps vcs.ErrBaseNotFound). An empty branch counts from the base.
// A working-copy commit with more than one parent has no one commit to
// publish, and is refused.
func (d Driver) pendingPush(ctx context.Context, s vcs.Slot, remote, branch string) (pending, error) {
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return pending{}, err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return pending{}, err
	}
	if len(wc.Commit.Parents) != 1 {
		return pending{}, fmt.Errorf("the working-copy commit of jj slot %s has %d parents, so no one commit under it is the slot's to publish; start the working copy on one with `jj new`", s.Path, len(wc.Commit.Parents))
	}
	p := pending{op: op, tip: wc.Commit.Parents[0]}
	marks, err := d.bookmarks(ctx, s.Path, op, branch, s.BaseRef)
	if err != nil {
		return pending{}, err
	}
	p.local = branch != "" && hasBookmark(marks, branch, "")
	if branch != "" && remote != "" && remote != gitRemote && hasBookmark(marks, branch, remote) {
		p.from = target{revset: remoteBookmark(branch, remote), name: branch + "@" + remote}
		p.upstream = true
	} else if p.from, err = resolveBase(marks, s.BaseRef, s.BaseRemote); err != nil {
		return pending{}, err
	}
	if p.commits, err = d.commits(ctx, s.Path, op, p.from.revset.to(workingCopy.parents())); err != nil {
		return pending{}, err
	}
	return p, nil
}

// unpushable reports the commits of p that remote does not hold yet, which
// jj git push would refuse to send: a commit with conflicts or with no
// description (plan D6), as a *vcs.UnpushableCommitsError naming them. What
// the remote already holds (the base's own commits, after a rebase onto a
// moved base) is not checked, as jj does not check it.
func (d Driver) unpushable(ctx context.Context, s vcs.Slot, p pending, remote string) error {
	unsent, err := d.commits(ctx, s.Path, p.op, p.from.revset.to(workingCopy.parents()).minus(remoteBookmarksOn(remote).ancestors()))
	if err != nil {
		return err
	}
	refused := &vcs.UnpushableCommitsError{}
	for _, c := range unsent {
		if c.Conflict {
			refused.Conflicted = append(refused.Conflicted, shortID(c.Commit.CommitID))
		}
		if c.Commit.Description == "" {
			refused.Undescribed = append(refused.Undescribed, shortID(c.Commit.CommitID))
		}
	}
	if len(refused.Conflicted) == 0 && len(refused.Undescribed) == 0 {
		return nil
	}
	return refused
}

// PlanPublish decides a publish of the slot, changing nothing:
//   - the branch X: the intent's, which the service fills with the slot's
//     manifest branch when the caller names none; with neither,
//     vcs.ErrNoBranch, since a jj slot is on no branch;
//   - the remote R: the intent's, else the one the slot pushes X to
//     (pushRemote), which the slot must have (*vcs.UnknownRemoteError);
//   - the commits to send (pendingPush): none is a
//     *vcs.NothingToPublishError, and nothing is pushed;
//   - of those, the ones R does not hold yet must be ones jj will push
//     (unpushable).
//
// The plan's Commit is `@-` as PlanPublish read it, for PreparePublish to
// set X on, and Create reports that the repository has no bookmark X yet.
// The intent's Explicit and Unmanaged change nothing: a jj slot's commits
// are always counted, and a `kind: local` store's slot is published as any
// other, under a bookmark.
func (d Driver) PlanPublish(ctx context.Context, s vcs.Slot, intent vcs.PublishIntent) (vcs.PublishPlan, error) {
	if intent.Branch == "" {
		return vcs.PublishPlan{}, vcs.ErrNoBranch
	}
	remote, err := d.pushRemote(ctx, s.Path, intent.Remote, intent.Branch)
	if err != nil {
		return vcs.PublishPlan{}, err
	}
	url, ok, err := d.remoteURL(ctx, s.Path, remote)
	if err != nil {
		return vcs.PublishPlan{}, fmt.Errorf("read the URL of remote %q in %s: %w", remote, s.Path, err)
	}
	if !ok {
		return vcs.PublishPlan{}, &vcs.UnknownRemoteError{Remote: remote, Resolved: intent.Remote == ""}
	}
	p, err := d.pendingPush(ctx, s, remote, intent.Branch)
	if err != nil {
		return vcs.PublishPlan{}, err
	}
	if len(p.commits) == 0 {
		return vcs.PublishPlan{}, &vcs.NothingToPublishError{Reason: p.nothing()}
	}
	if err := d.unpushable(ctx, s, p, remote); err != nil {
		return vcs.PublishPlan{}, err
	}
	return vcs.PublishPlan{Remote: remote, URL: url, Branch: intent.Branch, Create: !p.local, Commit: p.tip}, nil
}

// PreparePublish sets the plan's branch on the plan's Commit: `jj bookmark
// set X -r <commit>` in the slot, which creates the bookmark or moves it
// there. jj refuses to move a bookmark backwards or sideways, so one moved
// elsewhere since (by an agent, say) is left where it is, and that is jj's
// error; one a rebase moved along with the commits it rewrote is where the
// plan's Commit is, or behind it. A slot whose working copy no longer sits on
// the plan's Commit (an agent committed or rewrote meanwhile) is refused, so
// what is pushed is what PlanPublish checked; a new publish plans it afresh.
// If the push then fails, the bookmark stays, as a branch git's publish
// creates does, and a retry pushes it.
func (d Driver) PreparePublish(ctx context.Context, s vcs.Slot, plan vcs.PublishPlan) error {
	if plan.Branch == "" || plan.Commit == "" {
		return errors.New("a jj publish needs the branch and the commit its plan names")
	}
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return err
	}
	if len(wc.Commit.Parents) != 1 || wc.Commit.Parents[0] != plan.Commit {
		return fmt.Errorf("jj slot %s moved while its publish was checked: its working copy no longer sits on %s; publish again", s.Path, shortID(plan.Commit))
	}
	if _, err := d.jj.snapshot(ctx, s.Path, "bookmark", "set", "-r", commitRevision(plan.Commit).String(), "--", quote(plan.Branch)); err != nil {
		return fmt.Errorf("set bookmark %q in %s: %w", plan.Branch, s.Path, err)
	}
	return nil
}

// Publish pushes the plan's branch to its remote: `jj git push --remote R
// --bookmark X` in the slot, under ANGEE_GIT_TIMEOUT. A branch new to the
// remote is created there, and jj tracks it; one the remote already has is
// moved to the plan's commit, sideways too, as after a sync-base rebase
// rewrote what was published: jj then force-pushes it, with a lease that
// refuses when the remote branch moved since the slot last fetched it. jj's
// account of the push is the result's message, URL-redacted. Any refusal
// (the lease, a remote's hook, a commit the pre-check could not foresee: plan
// D6) is jj's *CommandError, whose standard error is redacted the same way.
func (d Driver) Publish(ctx context.Context, s vcs.Slot, plan vcs.PublishPlan) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	operation := fmt.Sprintf("jj git push --remote %s --bookmark %s", plan.Remote, plan.Branch)
	_, stderr, err := d.jj.networkOutputs(ctx, operation, s.Path, "git", "push", "--remote="+plan.Remote, "--bookmark="+exact(plan.Branch))
	result.Message = logctx.RedactText(strings.TrimSpace(string(stderr)))
	if err != nil {
		return result, err
	}
	result.OK = true
	return result, nil
}

// PlanPush plans a push of the slot as a publish to the remote it pushes to
// (plan phase 8 step 4): under ref when the caller names one, else under
// branch, the slot's manifest branch, refusing as PlanPublish refuses. A slot
// with nothing to send is skipped, saying why. With neither ref nor branch,
// a slot with commits beyond its base has no bookmark to push them under
// (vcs.ErrDetachedWithWork), and one without is skipped.
func (d Driver) PlanPush(ctx context.Context, s vcs.Slot, ref, branch string) (vcs.PushPlan, error) {
	if ref != "" {
		branch = ref
	}
	if branch == "" {
		p, err := d.pendingPush(ctx, s, "", "")
		if err != nil {
			return vcs.PushPlan{}, err
		}
		if len(p.commits) == 0 {
			return vcs.PushPlan{Skip: true, Reason: p.nothing()}, nil
		}
		return vcs.PushPlan{}, vcs.ErrDetachedWithWork
	}
	plan, err := d.PlanPublish(ctx, s, vcs.PublishIntent{Branch: branch})
	var nothing *vcs.NothingToPublishError
	if errors.As(err, &nothing) {
		return vcs.PushPlan{Skip: true, Reason: nothing.Reason}, nil
	}
	if err != nil {
		return vcs.PushPlan{}, err
	}
	return vcs.PushPlan{Track: branch, Publish: &plan}, nil
}

// Push carries out a plan PlanPush returned: a skipped plan pushes nothing,
// and any other publishes its Publish, as PreparePublish and Publish do.
func (d Driver) Push(ctx context.Context, s vcs.Slot, plan vcs.PushPlan) error {
	if plan.Skip {
		return nil
	}
	if plan.Publish == nil {
		return errors.New("a jj push needs the publish its plan carries")
	}
	if err := d.PreparePublish(ctx, s, *plan.Publish); err != nil {
		return err
	}
	_, err := d.Publish(ctx, s, *plan.Publish)
	return err
}
