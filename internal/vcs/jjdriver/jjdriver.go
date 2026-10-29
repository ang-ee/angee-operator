// Package jjdriver is the vcs.Driver for jj slots: secondary jj workspaces of
// a source's primary, colocated with git when the primary is. It makes and
// removes a slot (lifecycle.go) and reads its status and diff; every other
// verb waits for phase 8 of .agents/plans/jj-native-operator.md and reports
// vcs.ErrUnsupported. The service selects it for a slot whose mode is jj, and
// refuses those verbs on a jj slot itself before it asks the driver. Source
// captures also use it as the vcs.BackupLayer over a Git checkout, which
// plans nothing unless colocated JJ metadata is present.
//
// jj owns the slot: the lifecycle, status, diff and verbs run the jj CLI and
// interpret what it prints through the templates in templates.go. BackupLayerPlan additionally
// reads repository pointer files to validate portable topology without
// changing them.
// The driver never writes JJ metadata directly.
// Which remote a slot's branch is pushed to is git's answer, from the slot's
// colocated git config, as it is for git slots.
//
// The primary's lock (plan D9) is the caller's: EnsurePrimary (which creates
// the primary's .jj), Add and Remove (which add and forget a workspace, and
// drop its git worktree registration) run under it. jj merges
// concurrent operations on its own, so nothing else needs it: Exists, Holds,
// Elsewhere, ForgetName (of a slot whose directory is gone), Status, Diff and
// the slot verbs take none.
package jjdriver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/vcs"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// Driver reads and acts on jj slots through the jj CLI on PATH, and asks git
// for the push remote.
type Driver struct {
	jj  runner
	git git.Client
}

var _ vcs.Driver = Driver{}

// New returns a driver that runs jj from PATH, and git through client.
func New(client git.Client) Driver {
	return Driver{git: client}
}

// target is a revision Status counts against: its revset, and the name a
// message gives it.
type target struct {
	revset revset
	name   string
}

// Status reads the slot. Its snapshot, `jj status`, records the edits jj
// has recorded nothing of yet (plan D7), and reports the new files it left
// out (Q10). Every read after it is pinned to the operation that snapshot
// left the repository at (statusSnapshot), so they see what the report saw,
// and agree with each other whatever agents do meanwhile. A slot whose
// working copy another workspace rewrote updates itself (D8).
//
// For slot branch X (Slot.Branch), base B (Slot.BaseRef on Slot.BaseRemote,
// else origin, else the local bookmark) and R, the remote git pushes X to
// from the slot (git.Client.PushRemoteFor, as publish resolves it for git
// slots; a jj git.push setting plays no part):
//   - UntrackedPaths: the new paths `jj status` lists as untracked, files
//     over snapshot.max-new-file-size or outside snapshot.auto-track.
//   - Dirty: the working-copy commit `@` is not empty, or there are
//     UntrackedPaths.
//   - Upstream: X@R when the slot has a branch, git names R, and that remote
//     bookmark exists.
//   - Ahead and Behind: from `@-`, against Upstream when set, else B.
//   - BaseBehind: the commits of B that `@-` lacks.
//   - Conflicted and ConflictPaths: the conflicted commits of B..@.
//   - CurrentRef: X when local bookmark X points into B..@, else the short
//     commit id of `@-` (its first parent, if it has two).
//   - ChangeID: the change id of `@`.
//   - Pushed: not dirty and nothing ahead.
//
// A base that exists neither on its remote nor locally (vcs.ErrBaseNotFound)
// leaves out what only B bounds: BaseBehind, the conflicts and CurrentRef's
// X, and, with no upstream, Ahead and Behind. As the git driver does, Status
// keeps the verdict wherever it needs no base, for a dirty slot or one with
// an upstream; unlike it, it returns the verdict with a *vcs.CountError
// wrapping the base's error, since what B bounds is unread. A clean slot with
// no upstream has only B to measure its own commits by, so the base's error
// comes with no verdict.
func (d Driver) Status(ctx context.Context, s vcs.Slot) (vcs.Status, error) {
	var status vcs.Status
	untracked, op, err := d.statusSnapshot(ctx, s.Path)
	if err != nil {
		return status, err
	}
	wc, err := d.workingCopy(ctx, s.Path, op)
	if err != nil {
		return status, err
	}
	status.ChangeID = wc.Commit.ChangeID
	status.UntrackedPaths = untracked
	status.Dirty = !wc.Empty || len(untracked) > 0
	if len(wc.ParentsShort) > 0 {
		status.CurrentRef = wc.ParentsShort[0]
	}

	marks, err := d.bookmarks(ctx, s.Path, op, s.BaseRef, s.Branch)
	if err != nil {
		return status, err
	}
	upstream, hasUpstream, err := d.upstream(ctx, s.Path, s.Branch, marks)
	if err != nil {
		return status, err
	}
	if hasUpstream {
		status.Upstream = upstream.name
	}
	base, baseErr := resolveBase(marks, s.BaseRef, s.BaseRemote)
	if baseErr != nil && !errors.Is(baseErr, vcs.ErrBaseNotFound) {
		return status, baseErr
	}
	hasBase := baseErr == nil

	if hasBase {
		own, err := d.commits(ctx, s.Path, op, base.revset.to(workingCopy))
		if err != nil {
			return status, err
		}
		var conflictPaths []string
		for _, c := range own {
			if c.Conflict {
				status.Conflicted = true
				conflictPaths = append(conflictPaths, c.ConflictedFiles...)
			}
			if s.Branch != "" && c.hasLocalBookmark(s.Branch) {
				status.CurrentRef = s.Branch
			}
		}
		if status.Conflicted {
			slices.Sort(conflictPaths)
			status.ConflictPaths = slices.Compact(conflictPaths)
		}
	}

	from := workingCopy.parents()
	counted, hasCounted := base, hasBase
	if hasUpstream {
		counted, hasCounted = upstream, true
	}
	if hasCounted {
		if status.Ahead, err = d.count(ctx, s.Path, op, counted.revset.to(from)); err != nil {
			return status, err
		}
		if status.Behind, err = d.count(ctx, s.Path, op, from.to(counted.revset)); err != nil {
			return status, err
		}
	}
	if hasBase {
		status.BaseBehind = status.Behind
		if hasUpstream {
			if status.BaseBehind, err = d.count(ctx, s.Path, op, from.to(base.revset)); err != nil {
				return status, err
			}
		}
	}

	switch {
	case status.Dirty:
		status.UnpushedReason = dirtyReason(!wc.Empty, status.UntrackedPaths)
	case !hasCounted:
		return status, baseErr
	case status.Ahead > 0 && hasUpstream:
		status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of %s", status.Ahead, upstream.name)
	case status.Ahead > 0:
		status.UnpushedReason = fmt.Sprintf("%d commit(s) ahead of base ref %s with no upstream", status.Ahead, base.name)
	default:
		status.Pushed = true
	}
	if !hasBase {
		return status, &vcs.CountError{Err: baseErr}
	}
	return status, nil
}

// maxReasonPaths is how many untracked paths dirtyReason names, as many as
// the service names in a message.
const maxReasonPaths = 10

// dirtyReason says what a dirty slot holds that nothing else does: changes
// recorded in its working-copy commit, untracked paths, or both. A path with
// a character that does not print is quoted: jj prints names as they are,
// and the reason reaches terminals and API clients.
func dirtyReason(changed bool, untracked []string) string {
	if len(untracked) == 0 {
		return "uncommitted changes"
	}
	shown := untracked[:min(len(untracked), maxReasonPaths)]
	names := make([]string, len(shown))
	for i, path := range shown {
		if strings.ContainsFunc(path, func(r rune) bool { return !unicode.IsPrint(r) }) {
			path = strconv.Quote(path)
		}
		names[i] = path
	}
	reason := "untracked paths jj does not snapshot: " + strings.Join(names, ", ")
	if more := len(untracked) - len(shown); more > 0 {
		reason += fmt.Sprintf(" and %d more", more)
	}
	if changed {
		return "uncommitted changes, and " + reason
	}
	return reason
}

// resolveBase picks the slot's base from the bookmarks listed for it: the
// first of baseCandidates that exists.
func resolveBase(marks []bookmark, ref, remote string) (target, error) {
	if ref == "" {
		return target{}, fmt.Errorf("the slot has no base ref: %w", vcs.ErrBaseNotFound)
	}
	candidates := baseCandidates(ref, remote)
	for _, c := range candidates {
		if hasBookmark(marks, ref, c.remote) {
			return c.target, nil
		}
	}
	return target{}, fmt.Errorf("no bookmark %q on %s or locally: %w", ref, baseRemote(remote), vcs.ErrBaseNotFound)
}

// baseCandidate is a bookmark that can be a slot's base: ref on remote, local
// when remote is empty.
type baseCandidate struct {
	target
	remote string
}

// baseCandidates lists, in order, the bookmarks that can be the base B of a
// slot with base ref on remote: ref on remote (origin when remote is empty),
// then the local bookmark ref. jj's git pseudo-remote, which mirrors the
// colocated repository's own branches, is never the base's remote. Status
// counts against the first that exists, and Add starts a slot on it.
func baseCandidates(ref, remote string) []baseCandidate {
	var candidates []baseCandidate
	if remote = baseRemote(remote); remote != gitRemote {
		candidates = append(candidates, baseCandidate{
			target: target{revset: remoteBookmark(ref, remote), name: ref + "@" + remote},
			remote: remote,
		})
	}
	return append(candidates, baseCandidate{target: target{revset: localBookmark(ref), name: ref}})
}

// baseRemote is the remote a base ref comes from: remote, else origin.
func baseRemote(remote string) string {
	if remote == "" {
		return "origin"
	}
	return remote
}

// upstream returns branch on the remote git pushes it to from the slot, when
// that remote bookmark exists. It reports none when git names no push remote
// for it (several remotes, none of them origin, and no push default), and for
// a slot that is not colocated with git, which has no git config of its own
// to ask: git run there would read whatever repository encloses it.
func (d Driver) upstream(ctx context.Context, dir, branch string, marks []bookmark) (target, bool, error) {
	if branch == "" {
		return target{}, false, nil
	}
	if colocated, err := colocated(dir); err != nil || !colocated {
		return target{}, false, err
	}
	remote, err := d.git.PushRemoteFor(ctx, dir, branch)
	var none *git.NoPushRemoteError
	if errors.As(err, &none) {
		return target{}, false, nil
	}
	if err != nil {
		return target{}, false, fmt.Errorf("resolve the push remote of %q in %s: %w", branch, dir, err)
	}
	if remote == gitRemote || !hasBookmark(marks, branch, remote) {
		return target{}, false, nil
	}
	return target{revset: remoteBookmark(branch, remote), name: branch + "@" + remote}, true, nil
}

// hasBookmark reports whether marks holds bookmark name on remote (local
// when remote is empty), pointing somewhere.
func hasBookmark(marks []bookmark, name, remote string) bool {
	return slices.ContainsFunc(marks, func(m bookmark) bool {
		return m.Name == name && m.Remote == remote && m.present()
	})
}

// operationArgs print the repository's latest operation.
var operationArgs = []string{"op", "log", "--no-graph", "-n", "1", "-T", operationTemplate}

// snapshot snapshots the slot's working copy and returns the operation that
// recorded it: `jj op log` snapshots before it reads. It is the one command of
// Diff that snapshots; the reads after it are pinned to its operation. Status
// snapshots through `jj status` instead (statusSnapshot).
func (d Driver) snapshot(ctx context.Context, dir string) (string, error) {
	out, err := d.jj.snapshot(ctx, dir, operationArgs...)
	if err != nil {
		return "", err
	}
	op := strings.TrimSpace(string(out))
	if op == "" || strings.ContainsAny(op, " \n") {
		return "", fmt.Errorf("jj op log in %s printed %q, want one operation id", dir, out)
	}
	return op, nil
}

// maxSnapshotAttempts bounds how often statusSnapshot snapshots a slot while
// other jj commands keep recording operations in its repository.
const maxSnapshotAttempts = 3

// statusSnapshot snapshots the slot through `jj status` (untracked) and
// returns the untracked paths its report lists, with the operation that
// report describes, for the reads that follow to be pinned to.
//
// `jj status` does not say which operation it ended at, and another jj
// command can record one before or after it. So statusSnapshot reads the
// latest operation before the snapshot (A) and after it (C), and takes C only
// when nothing but that snapshot came between (settled): C is A, since the
// snapshot found nothing to record, or C is a snapshot whose only parent is
// A. Otherwise it snapshots again, from C, up to maxSnapshotAttempts times,
// then fails. `jj status` itself records an operation that is not a snapshot
// when it updates a stale working copy (`reset git head`) or imports what git
// changed in the colocated repository (`import git refs`); each costs one
// more snapshot.
func (d Driver) statusSnapshot(ctx context.Context, dir string) ([]string, string, error) {
	before, err := d.latestOperation(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	for attempt := 1; ; attempt++ {
		untracked, err := d.untracked(ctx, dir)
		if err != nil {
			return nil, "", err
		}
		after, err := d.latestOperation(ctx, dir)
		if err != nil {
			return nil, "", err
		}
		if settled(before, after) {
			return untracked, after.ID, nil
		}
		if attempt == maxSnapshotAttempts {
			return nil, "", fmt.Errorf("jj status in %s: other jj commands recorded operations during each of %d snapshots; retry once they finish", dir, attempt)
		}
		before = after
	}
}

// settled reports whether after, the latest operation once a snapshot ran,
// is before or the snapshot's own: nothing else was recorded in between.
func settled(before, after operationEntry) bool {
	return after.ID == before.ID || after.IsSnapshot && slices.Equal(after.Parents, []string{before.ID})
}

// latestOperation reads the repository's latest operation without
// snapshotting.
func (d Driver) latestOperation(ctx context.Context, dir string) (operationEntry, error) {
	out, err := d.jj.latest(ctx, dir, "op", "log", "--no-graph", "-n", "1", "-T", operationEntryTemplate)
	if err != nil {
		return operationEntry{}, err
	}
	return decodeOperation(dir, out)
}

// decodeOperation reads the one operation `jj op log -n 1` printed in dir.
func decodeOperation(dir string, out []byte) (operationEntry, error) {
	ops, err := decodeLines[operationEntry](out)
	if err != nil {
		return operationEntry{}, fmt.Errorf("jj op log in %s: %w", dir, err)
	}
	if len(ops) != 1 || ops[0].ID == "" {
		return operationEntry{}, fmt.Errorf("jj op log in %s printed %q, want one operation", dir, out)
	}
	return ops[0], nil
}

// workingCopy reads the slot's working-copy commit at operation op.
func (d Driver) workingCopy(ctx context.Context, dir, op string) (commitEntry, error) {
	entries, err := d.commits(ctx, dir, op, workingCopy)
	if err != nil {
		return commitEntry{}, err
	}
	if len(entries) != 1 {
		return commitEntry{}, fmt.Errorf("jj log -r @ in %s printed %d commits, want 1", dir, len(entries))
	}
	return entries[0], nil
}

// bookmarks lists the local and remote bookmarks named in names, on every
// remote, at operation op. Empty names are skipped; with none left it lists
// nothing.
func (d Driver) bookmarks(ctx context.Context, dir, op string, names ...string) ([]bookmark, error) {
	args := []string{"bookmark", "list", "--all-remotes", "-T", bookmarksTemplate}
	listed := false
	for _, name := range names {
		if name != "" {
			args = append(args, exact(name))
			listed = true
		}
	}
	if !listed {
		return nil, nil
	}
	out, err := d.jj.read(ctx, dir, op, args...)
	if err != nil {
		return nil, err
	}
	return decodeLines[bookmark](out)
}

// commits lists the commits of r at operation op.
func (d Driver) commits(ctx context.Context, dir, op string, r revset) ([]commitEntry, error) {
	out, err := d.jj.read(ctx, dir, op, "log", "--no-graph", "-r", r.String(), "-T", commitsTemplate)
	if err != nil {
		return nil, err
	}
	return decodeLines[commitEntry](out)
}

// count counts the commits of r at operation op.
func (d Driver) count(ctx context.Context, dir, op string, r revset) (int, error) {
	out, err := d.jj.read(ctx, dir, op, "log", "--no-graph", "-r", r.String(), "-T", countTemplate)
	if err != nil {
		return 0, err
	}
	return len(out), nil
}

// Diff returns the slot's uncommitted changes (its working-copy commit
// against its parents) when ref is empty, else the committed range from the
// slot to ref: the tree of `@`'s first parent against ref's, leaving
// uncommitted changes out, as the git driver leaves out the working tree.
// ref is one revision as jj resolves a name (a bookmark, tag, commit or
// change id), quoted, so a remote bookmark cannot be named through it yet.
// The slot is snapshotted first either way.
func (d Driver) Diff(ctx context.Context, s vcs.Slot, ref string) ([]api.DiffFile, error) {
	op, err := d.snapshot(ctx, s.Path)
	if err != nil {
		return nil, err
	}
	args := []string{"diff", "--git"}
	if ref != "" {
		args = append(args, "--from", workingCopy.firstParent().String(), "--to", symbol(ref).String())
	}
	out, err := d.jj.read(ctx, s.Path, op, args...)
	if err != nil {
		return nil, err
	}
	files, _, err := gitdiff.Parse(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("parse jj diff in %s: %w", s.Path, err)
	}
	return vcs.DiffFiles(files), nil
}
