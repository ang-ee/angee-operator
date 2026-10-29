package jjdriver

import (
	"context"
	"errors"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestVerbsNotImplementedYet refuses every verb phase 8 has not implemented,
// and the reads that guard them, with vcs.ErrUnsupported, before running
// anything: no jj is needed. So are the verbs that drive a merge or rebase
// stopped on conflicts, which a jj slot never has, a sync by merging, and the
// preconditions of a verb other than sync-base.
func TestVerbsNotImplementedYet(t *testing.T) {
	ctx := context.Background()
	d := driver()
	s := vcs.Slot{Path: t.TempDir()}
	for name, call := range map[string]func() error{
		"BranchMismatch": func() error { _, err := d.BranchMismatch(ctx, s); return err },
		"DirtyPaths":     func() error { _, err := d.DirtyPaths(ctx, s); return err },
		"Pull":           func() error { return d.Pull(ctx, s) },
		"PlanPush":       func() error { _, err := d.PlanPush(ctx, s, "", ""); return err },
		"Push":           func() error { return d.Push(ctx, s, vcs.PushPlan{}) },
		"Merge":          func() error { _, err := d.Merge(ctx, s, "main"); return err },
		"MergeAbort":     func() error { _, err := d.MergeAbort(ctx, s); return err },
		"RebaseAbort":    func() error { _, err := d.RebaseAbort(ctx, s); return err },
		"RebaseContinue": func() error { _, err := d.RebaseContinue(ctx, s); return err },
		"PlanPublish":    func() error { _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{}); return err },
		"PreparePublish": func() error { return d.PreparePublish(ctx, s, vcs.PublishPlan{}) },
		"Publish":        func() error { _, err := d.Publish(ctx, s, vcs.PublishPlan{}); return err },
		"SyncBase merge": func() error { _, err := d.SyncBase(ctx, s, "main", vcs.SyncMerge); return err },
		"SyncMethod":     func() error { _, err := d.SyncMethod(vcs.SyncMerge); return err },
		"Ready publish":  func() error { return d.Ready(ctx, s, "publish") },
	} {
		if err := call(); !errors.Is(err, vcs.ErrUnsupported) {
			t.Errorf("%s() error = %v, want vcs.ErrUnsupported", name, err)
		}
	}
	for _, method := range []string{vcs.SyncDefault, vcs.SyncRebase} {
		if got, err := d.SyncMethod(method); err != nil || got != vcs.SyncRebase {
			t.Errorf("SyncMethod(%q) = %q, %v, want a rebase", method, got, err)
		}
	}
	if err := d.Ready(ctx, s, vcs.VerbSyncBase); err != nil {
		t.Errorf("Ready(sync-base) error = %v, want none: the working copy rides along", err)
	}
	if url := d.FetchURL(ctx, s); url != "" {
		t.Errorf("FetchURL() of a slot without git = %q, want none", url)
	}
	if url := d.PushURL(ctx, s, "origin"); url != "" {
		t.Errorf("PushURL() = %q, want none", url)
	}
}
