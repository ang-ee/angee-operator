package jjdriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestVerbsAJJSlotDoesNotRun refuses, with vcs.ErrUnsupported and before
// running anything (no jj is needed), what has no meaning on a jj slot: a
// pull and its preconditions, since jj takes a base in by rebasing; the verbs
// that drive a merge or rebase stopped on conflicts, which a jj slot never
// has; a sync by merging; and the preconditions of a verb the driver does not
// know. Destroy and repair have none, and run nothing either.
func TestVerbsAJJSlotDoesNotRun(t *testing.T) {
	ctx := context.Background()
	d := driver()
	s := vcs.Slot{Path: t.TempDir()}
	for name, call := range map[string]func() error{
		"Pull":           func() error { return d.Pull(ctx, s) },
		"Ready pull":     func() error { return d.Ready(ctx, s, vcs.VerbPull) },
		"MergeAbort":     func() error { _, err := d.MergeAbort(ctx, s); return err },
		"RebaseAbort":    func() error { _, err := d.RebaseAbort(ctx, s); return err },
		"RebaseContinue": func() error { _, err := d.RebaseContinue(ctx, s); return err },
		"SyncBase merge": func() error { _, err := d.SyncBase(ctx, s, "main", vcs.SyncMerge); return err },
		"SyncMethod":     func() error { _, err := d.SyncMethod(vcs.SyncMerge); return err },
		"Ready migrate":  func() error { return d.Ready(ctx, s, "migrate") },
	} {
		if err := call(); !errors.Is(err, vcs.ErrUnsupported) {
			t.Errorf("%s() error = %v, want vcs.ErrUnsupported", name, err)
		}
	}
	if err := d.Pull(ctx, s); err == nil || !errors.Is(err, vcs.ErrUnsupported) || !strings.Contains(err.Error(), "never pulled") || !strings.Contains(err.Error(), "sync-base") {
		t.Errorf("Pull() error = %v, want it to say a jj slot is never pulled and name sync-base", err)
	}
	for _, verb := range []vcs.Verb{vcs.VerbDestroy, vcs.VerbRepair} {
		if err := d.Ready(ctx, s, verb); err != nil {
			t.Errorf("Ready(%s) of a directory with no jj error = %v, want none, and nothing run", verb, err)
		}
	}
	for _, method := range []string{vcs.SyncDefault, vcs.SyncRebase} {
		if got, err := d.SyncMethod(method); err != nil || got != vcs.SyncRebase {
			t.Errorf("SyncMethod(%q) = %q, %v, want a rebase", method, got, err)
		}
	}
	if url := d.FetchURL(ctx, s); url != "" {
		t.Errorf("FetchURL() of a slot without git = %q, want none", url)
	}
	if url := d.PushURL(ctx, s, ""); url != "" {
		t.Errorf("PushURL() of a slot without git = %q, want none", url)
	}
}
