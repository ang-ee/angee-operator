package jjdriver

import (
	"context"
	"errors"
	"testing"

	"github.com/ang-ee/angee-operator/internal/vcs"
)

// TestVerbsNotImplementedYet refuses every verb phase 8 has not implemented,
// the reads that guard them, and the slot lifecycle phase 5 step 6 adds, with
// vcs.ErrUnsupported, before running anything: no jj is needed.
func TestVerbsNotImplementedYet(t *testing.T) {
	ctx := context.Background()
	d := driver()
	s := vcs.Slot{Path: t.TempDir()}
	for name, call := range map[string]func() error{
		"Exists":         func() error { _, err := d.Exists(ctx, s); return err },
		"Add":            func() error { return d.Add(ctx, s, nil, nil) },
		"Remove":         func() error { return d.Remove(ctx, s, nil) },
		"BranchMismatch": func() error { _, err := d.BranchMismatch(ctx, s); return err },
		"DirtyPaths":     func() error { _, err := d.DirtyPaths(ctx, s); return err },
		"ResolveBase":    func() error { _, err := d.ResolveBase(ctx, s); return err },
		"Fetch":          func() error { return d.Fetch(ctx, s) },
		"Pull":           func() error { return d.Pull(ctx, s) },
		"PlanPush":       func() error { _, err := d.PlanPush(ctx, s, "", ""); return err },
		"Push":           func() error { return d.Push(ctx, s, vcs.PushPlan{}) },
		"SyncBase":       func() error { _, err := d.SyncBase(ctx, s, "main", vcs.SyncDefault); return err },
		"Merge":          func() error { _, err := d.Merge(ctx, s, "main"); return err },
		"Rebase":         func() error { _, err := d.Rebase(ctx, s, "main"); return err },
		"MergeAbort":     func() error { _, err := d.MergeAbort(ctx, s); return err },
		"RebaseAbort":    func() error { _, err := d.RebaseAbort(ctx, s); return err },
		"RebaseContinue": func() error { _, err := d.RebaseContinue(ctx, s); return err },
		"PlanPublish":    func() error { _, err := d.PlanPublish(ctx, s, vcs.PublishIntent{}); return err },
		"PreparePublish": func() error { return d.PreparePublish(ctx, s, vcs.PublishPlan{}) },
		"Publish":        func() error { _, err := d.Publish(ctx, s, vcs.PublishPlan{}); return err },
	} {
		if err := call(); !errors.Is(err, vcs.ErrUnsupported) {
			t.Errorf("%s() error = %v, want vcs.ErrUnsupported", name, err)
		}
	}
	if url := d.FetchURL(ctx, s); url != "" {
		t.Errorf("FetchURL() = %q, want none", url)
	}
	if url := d.PushURL(ctx, s, "origin"); url != "" {
		t.Errorf("PushURL() = %q, want none", url)
	}
}
