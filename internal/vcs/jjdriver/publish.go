package jjdriver

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// whether it has that remote, from git's config: the slot's own when it is
// colocated, else that of the repository jj keeps its store in (`jj git
// root`), so the name is matched exactly and the URL is what git reads, as
// for a git slot. jj's git pseudo-remote, which mirrors the colocated
// repository's own branches, is no remote to push to.
func (d Driver) remoteURL(ctx context.Context, dir, remote string) (string, bool, error) {
	if remote == gitRemote {
		return "", false, nil
	}
	repo := dir
	if ok, err := colocated(dir); err != nil {
		return "", false, err
	} else if !ok {
		out, err := d.jj.store(ctx, dir, "git", "root")
		if err != nil {
			return "", false, err
		}
		if repo = strings.TrimSpace(string(out)); repo == "" {
			return "", false, fmt.Errorf("jj git root in %s printed no repository", dir)
		}
	}
	return d.git.RemoteURL(ctx, repo, remote)
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
	marks   []bookmark
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
		return pending{}, severalParents(s.Path, len(wc.Commit.Parents), "publish")
	}
	p := pending{op: op, tip: wc.Commit.Parents[0]}
	marks, err := d.bookmarks(ctx, s.Path, op, branch, s.BaseRef)
	if err != nil {
		return pending{}, err
	}
	p.marks = marks
	p.local = branch != "" && hasBookmark(marks, branch, "")
	if branch != "" && remote != "" && remote != gitRemote && hasBookmark(marks, branch, remote) {
		// A remote branch the repository does not track is someone else's
		// (fetched, never published from here): jj will not push over it,
		// and counting from it would measure the slot against their work.
		if !tracked(marks, branch, remote) {
			return pending{}, &vcs.BranchTakenError{Branch: branch, Remote: remote}
		}
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

// tracked reports whether the repository tracks bookmark name on remote: jj
// gives a remote bookmark a tracking target only when it does.
func tracked(marks []bookmark, name, remote string) bool {
	return slices.ContainsFunc(marks, func(m bookmark) bool {
		return m.Name == name && m.Remote == remote && m.TrackingTarget != nil
	})
}

// severalParents is a *vcs.NotReadyError for a slot whose working-copy commit
// has parents other than one (`jj new A B` makes two, as does a merge whose
// fresh working copy could not be started), so no one commit under it is the
// slot's to verb. `jj new` starts a working copy on it, which puts one commit
// under the working copy.
func severalParents(dir string, parents int, verb string) error {
	return &vcs.NotReadyError{
		Reason: fmt.Sprintf("its working-copy commit has %d parents, so no one commit under it is the slot's to %s", parents, verb),
		Fix:    fmt.Sprintf("jj -R %s new", dir),
	}
}

// ownBranch refuses a branch that holds what is not the slot's to publish
// over, read at p's snapshot. The slot's own commits are `B..@-` (`(X@R |
// X)..@-` when the base does not resolve).
//   - A branch X the caller names that exists, as bookmark X or as X@R, and
//     is not the slot's manifest branch (Slot.Branch), must be the slot's:
//     bookmark X only on the slot's own commits, under no other workspace's
//     working copy, and `B..X@R` not empty and only versions of the slot's
//     own (same change). Else it wraps vcs.ErrBranchExists, as git's
//     publish refuses a branch that exists, so `develop` is never moved onto
//     the slot's commits, deleted here or not.
//   - X@R may hold old versions of the slot's own commits, as a sync-base
//     rebase leaves it, but no other change: a commit of `@-..X@R` whose
//     change none of the slot's own has (a collaborator's, fetched) is a
//     *vcs.BranchMovedError naming it.
//   - Bookmark X must be on the slot's history (`X ~ ::@-` empty). A target
//     off it is work the slot does not hold: a collaborator's version of the
//     slot's own change, fetched beside the rebased one, which leaves X
//     conflicted between them, or another workspace's. Setting X would drop
//     it, jj's lease passing since it was fetched; that is a
//     *vcs.BranchMovedError too, naming what the slot can merge to take the
//     targets in as one commit (mergeableStray): X@R when they are R's, X
//     when it is not conflicted, X on another remote that holds them, else
//     the stray heads. After a merge of it, both targets are in the slot's
//     history, and the publish goes on.
func (d Driver) ownBranch(ctx context.Context, s vcs.Slot, p pending, remote, branch string) error {
	if !p.local && !p.upstream {
		return nil
	}
	head := workingCopy.parents()
	remoteHead := remoteBookmark(branch, remote)
	var heads []revset
	if p.upstream {
		heads = append(heads, remoteHead)
	}
	if p.local {
		heads = append(heads, localBookmark(branch))
	}
	base, baseErr := resolveBase(p.marks, s.BaseRef, s.BaseRemote)
	slots := union(heads...).to(head)
	if baseErr == nil {
		slots = base.revset.to(head)
	}
	ours, err := d.changes(ctx, s.Path, p.op, slots)
	if err != nil {
		return err
	}
	foreign := func(cs []commit) []string {
		var ids []string
		for _, c := range cs {
			if !slices.ContainsFunc(ours, func(o commit) bool { return o.ChangeID == c.ChangeID }) {
				ids = append(ids, shortID(c.CommitID))
			}
		}
		return ids
	}

	if branch != s.Branch {
		exists := fmt.Errorf("branch %q holds commits that are not the slot's own: %w", branch, vcs.ErrBranchExists)
		if baseErr != nil {
			return exists
		}
		if p.local {
			own := base.revset.to(head).minus(otherWorkingCopies.ancestors())
			if stray, err := d.count(ctx, s.Path, p.op, localBookmark(branch).minus(own)); err != nil {
				return err
			} else if stray > 0 {
				return exists
			}
		}
		if p.upstream {
			onRemote, err := d.changes(ctx, s.Path, p.op, base.revset.to(remoteHead))
			if err != nil {
				return err
			}
			if len(onRemote) == 0 || len(foreign(onRemote)) > 0 {
				return exists
			}
		}
	}
	if p.upstream {
		theirs, err := d.changes(ctx, s.Path, p.op, head.to(remoteHead))
		if err != nil {
			return err
		}
		if ids := foreign(theirs); len(ids) > 0 {
			return &vcs.BranchMovedError{Branch: branch, Remote: remote, Commits: ids}
		}
	}
	if p.local {
		off := localBookmark(branch).minus(head.ancestors())
		stray, err := d.changes(ctx, s.Path, p.op, off)
		if err != nil {
			return err
		}
		if len(stray) > 0 {
			moved := &vcs.BranchMovedError{Branch: branch, Commits: shortIDs(commitIDs(stray))}
			if err := d.mergeableStray(ctx, s.Path, p, off, remote, len(stray), moved); err != nil {
				return err
			}
			return moved
		}
	}
	return nil
}

// mergeableStray names, in moved, what the slot can merge to take in the
// stray commits of bookmark X (off, `X ~ ::@-`, holding strays of them), so
// the refusal's hint names a ref that is one commit holding them all:
//   - X@R, the remote the publish goes to, when its branch holds them all;
//   - else X itself, when it is not conflicted (Remote stays empty);
//   - else X on another remote whose branch holds them all, as when the slot
//     publishes to a second remote and X moved on the first: X is then
//     conflicted between the slot's version and the collaborator's, and
//     names no one commit to merge (jj: "Name 'X' is conflicted");
//   - else the stray commits no other stray descends from (Heads), by
//     commit id, each of which the slot merges in turn.
func (d Driver) mergeableStray(ctx context.Context, dir string, p pending, off revset, remote string, strays int, moved *vcs.BranchMovedError) error {
	holds := func(r string) (bool, error) {
		held, err := d.count(ctx, dir, p.op, off.and(remoteBookmark(moved.Branch, r).ancestors()))
		return held == strays, err
	}
	if p.upstream {
		if ok, err := holds(remote); err != nil || ok {
			if ok {
				moved.Remote = remote
			}
			return err
		}
	}
	var others []string
	for _, m := range p.marks {
		switch {
		case m.Name != moved.Branch || !m.present():
		case m.Remote == "" && len(m.Target) == 1:
			return nil
		case m.Remote != "" && m.Remote != remote && m.Remote != gitRemote && len(m.Target) == 1 && !slices.Contains(others, m.Remote):
			others = append(others, m.Remote)
		}
	}
	slices.Sort(others)
	for _, other := range others {
		if ok, err := holds(other); err != nil || ok {
			if ok {
				moved.Remote = other
			}
			return err
		}
	}
	heads, err := d.changes(ctx, dir, p.op, off.heads())
	if err != nil {
		return err
	}
	moved.Heads = shortIDs(commitIDs(heads))
	return nil
}

// commitIDs lists the full ids of cs.
func commitIDs(cs []commit) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.CommitID
	}
	return ids
}

// changes lists the commits of r at operation op by their ids alone, for a
// set too large to read through commitsTemplate cheaply.
func (d Driver) changes(ctx context.Context, dir, op string, r revset) ([]commit, error) {
	out, err := d.jj.read(ctx, dir, op, "log", "--no-graph", "-r", r.String(), "-T", commitTemplate)
	if err != nil {
		return nil, err
	}
	return decodeLines[commit](out)
}

// checkedOut refuses branch when a git worktree of the slot's primary other
// than the slot has it checked out: setting the bookmark moves the branch,
// which leaves that worktree on no branch, as a rebase would (plan D12).
func (d Driver) checkedOut(ctx context.Context, s vcs.Slot, branch string) error {
	worktrees, err := d.primaryWorktrees(ctx, s.Path)
	if err != nil {
		return err
	}
	own := vcs.CanonicalPath(s.Path)
	for _, w := range worktrees {
		if w.Branch == branch && vcs.CanonicalPath(w.Path) != own {
			return &vcs.BranchTakenError{Branch: branch, Worktree: w.Path}
		}
	}
	return nil
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
//   - X must be the slot's to set: not checked out by another git worktree
//     of the primary (checkedOut), and, when the remote has it, tracked
//     there; else a *vcs.BranchTakenError;
//   - the commits to send (pendingPush): none is a
//     *vcs.NothingToPublishError, and nothing is pushed;
//   - X must hold nothing that is not the slot's (ownBranch): an existing
//     bookmark the caller names that is not the slot's wraps
//     vcs.ErrBranchExists, and work on X@R or X the slot lacks is a
//     *vcs.BranchMovedError;
//   - of the commits to send, the ones R does not hold yet must be ones jj
//     will push (unpushable).
//
// The plan's Commit is `@-` as PlanPublish read it, for PreparePublish to
// set X on, and Create reports that the repository has no bookmark X yet.
// RecordRemote is set when the caller names a remote other than the one git
// pushes X to by default, in a slot colocated with git, so Publish records it
// as X's push remote; Unmanaged carries the intent's, for a `kind: local`
// store's slot, whose config Publish leaves alone.
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
	if err := d.checkedOut(ctx, s, intent.Branch); err != nil {
		return vcs.PublishPlan{}, err
	}
	p, err := d.pendingPush(ctx, s, remote, intent.Branch)
	if err != nil {
		return vcs.PublishPlan{}, err
	}
	if len(p.commits) == 0 {
		return vcs.PublishPlan{}, &vcs.NothingToPublishError{Reason: p.nothing()}
	}
	if err := d.ownBranch(ctx, s, p, remote, intent.Branch); err != nil {
		return vcs.PublishPlan{}, err
	}
	if err := d.unpushable(ctx, s, p, remote); err != nil {
		return vcs.PublishPlan{}, err
	}
	plan := vcs.PublishPlan{Remote: remote, URL: url, Branch: intent.Branch, Create: !p.local, Commit: p.tip, Unmanaged: intent.Unmanaged}
	if intent.Remote != "" {
		if plan.RecordRemote, err = d.otherRemote(ctx, s.Path, intent.Branch, remote); err != nil {
			return vcs.PublishPlan{}, err
		}
	}
	return plan, nil
}

// otherRemote reports whether remote is not the one git pushes branch to by
// default from the slot (PushRemoteFor; none counts as other), for a slot
// colocated with git, whose config Publish can record it in. A slot that is
// not colocated has no config of its own, and is never told to record one.
func (d Driver) otherRemote(ctx context.Context, dir, branch, remote string) (bool, error) {
	if ok, err := colocated(dir); err != nil || !ok {
		return false, err
	}
	def, err := d.git.PushRemoteFor(ctx, dir, branch)
	var none *git.NoPushRemoteError
	switch {
	case errors.As(err, &none):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("resolve the push remote of %q in %s: %w", branch, dir, err)
	}
	return def != remote, nil
}

// PreparePublish sets the plan's branch on the plan's Commit: `jj bookmark
// set X -r <commit>` in the slot, which creates the bookmark or moves it
// there. jj refuses to move a bookmark backwards or sideways, so one moved
// elsewhere since (by an agent, say) is left where it is, and that is jj's
// error; one a rebase moved along with the commits it rewrote is where the
// plan's Commit is, or behind it. A slot whose working copy no longer sits on
// the plan's Commit (an agent committed or rewrote meanwhile) is refused,
// wrapping vcs.ErrCheckoutMoved, so what is pushed is what PlanPublish
// checked; a new publish plans it afresh.
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
		return fmt.Errorf("jj slot %s: its working copy no longer sits on %s, the commit its publish checked: %w", s.Path, shortID(plan.Commit), vcs.ErrCheckoutMoved)
	}
	if _, err := d.jj.snapshot(ctx, s.Path, "bookmark", "set", "-r", commitOf(plan.Commit).String(), "--", quote(plan.Branch)); err != nil {
		return fmt.Errorf("set bookmark %q in %s: %w", plan.Branch, s.Path, err)
	}
	return nil
}

// Publish pushes the plan's branch to its remote: `jj git push --remote R
// --bookmark X` in the slot, each matched exactly, under ANGEE_GIT_TIMEOUT. A branch new to the
// remote is created there, and jj tracks it; one the remote already has is
// moved to the plan's commit, sideways too, as after a sync-base rebase
// rewrote what was published: jj then force-pushes it, with a lease that
// refuses when the remote branch moved since the slot last fetched it. jj's
// account of the push is the result's message, URL-redacted. Any refusal
// (the lease, a remote's hook, a commit the pre-check could not foresee: plan
// D6) is jj's *CommandError, whose standard error is redacted the same way.
//
// Once pushed, a plan that says so records the remote as the branch's push
// remote (`branch.<X>.pushRemote` in the slot's shared git config), as `git
// push --set-upstream` records an upstream, so Status and later pushes
// follow it; it replaces whatever the branch had, as git's would, since the
// caller's latest publish names where the branch goes. The git config of a
// `kind: local` store (plan.Unmanaged) is the user's, which angee never
// changes: the remote is not recorded there, and Status keeps counting
// against the default push remote, which a warning says. Then X@R is read
// back: one that is not the plan's commit, or a record that failed, is a
// *vcs.Warning beside the OK result.
func (d Driver) Publish(ctx context.Context, s vcs.Slot, plan vcs.PublishPlan) (api.GitOpResult, error) {
	result := api.GitOpResult{ConflictFiles: []string{}}
	operation := fmt.Sprintf("jj git push --remote %s --bookmark %s", plan.Remote, plan.Branch)
	// Both are patterns to jj (glob by default), so each is matched exactly.
	_, stderr, err := d.jj.networkOutputs(ctx, operation, s.Path, "git", "push", "--remote="+exact(plan.Remote), "--bookmark="+exact(plan.Branch))
	result.Message = logctx.RedactText(strings.TrimSpace(string(stderr)))
	if err != nil {
		return result, err
	}
	result.OK = true
	var warned []string
	switch {
	case plan.RecordRemote && plan.Unmanaged:
		warned = append(warned, fmt.Sprintf("Pushed to %s, but not recorded as the push remote of %s: angee does not change a local store's git config, so status counts %s against its default push remote.", plan.Remote, plan.Branch, plan.Branch))
	case plan.RecordRemote:
		if err := d.git.SetBranchPushRemote(ctx, s.Path, plan.Branch, plan.Remote); err != nil {
			warned = append(warned, fmt.Sprintf("Pushed, but recording %s as the push remote of %s failed, so status counts it against the default remote: %v", plan.Remote, plan.Branch, err))
		}
	}
	if plan.Commit != "" {
		if where, err := d.pushedAt(ctx, s.Path, plan); err != nil {
			warned = append(warned, fmt.Sprintf("Pushed, but reading %s@%s after the push failed: %v", plan.Branch, plan.Remote, err))
		} else if !slices.Equal(where, []string{plan.Commit}) {
			warned = append(warned, fmt.Sprintf("Pushed, but jj reads %s@%s at %s, not at %s, the commit the publish checked.", plan.Branch, plan.Remote, strings.Join(shortIDs(where), ", "), shortID(plan.Commit)))
		}
	}
	if len(warned) > 0 {
		text := logctx.RedactText(strings.Join(warned, " "))
		result.Message = strings.TrimSpace(result.Message + "\n" + text)
		return result, &vcs.Warning{Text: text}
	}
	return result, nil
}

// pushedAt reads where the repository has the plan's branch on its remote,
// at the latest operation.
func (d Driver) pushedAt(ctx context.Context, dir string, plan vcs.PublishPlan) ([]string, error) {
	op, err := d.latestOperation(ctx, dir)
	if err != nil {
		return nil, err
	}
	marks, err := d.bookmarks(ctx, dir, op.ID, plan.Branch)
	if err != nil {
		return nil, err
	}
	for _, m := range marks {
		if m.Name == plan.Branch && m.Remote == plan.Remote {
			return m.Target, nil
		}
	}
	return nil, nil
}

// shortIDs cuts each of ids to the length jj shows.
func shortIDs(ids []string) []string {
	short := make([]string, len(ids))
	for i, id := range ids {
		short[i] = shortID(id)
	}
	return short
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
	return vcs.PushPlan{Publish: &plan}, nil
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
