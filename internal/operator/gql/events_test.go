package gql

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/service"
)

func TestEventHubSubscribeAfterStopReturnsClosedChan(t *testing.T) {
	// EventHub.Stop is documented to leave subsequent subscribes with a
	// pre-closed channel rather than spawning an orphan broker against a
	// cancelled root context.
	h := NewEventHub(nil)
	h.Stop()

	ch := h.SubscribeWorkspaceStatus(context.Background(), "anything")
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected pre-closed channel, received a value")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after Stop")
	}
}

func TestEventHubStartIsIdempotent(t *testing.T) {
	// Concurrent Start calls must not race a second pollTopology goroutine
	// into the wait group.
	h := NewEventHub(nil)
	h.SetPollInterval(50 * time.Millisecond)
	defer h.Stop()

	done := make(chan struct{})
	for range 8 {
		go func() {
			h.Start()
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	// If Start spawned multiple pollers, Stop's wg.Wait would still terminate
	// (cancel propagates), but the second goroutine could double-call Done
	// and panic. The sync.Once guard prevents that; reaching this assertion
	// without panicking is the win condition.
}

func TestReportPollErrorTransitions(t *testing.T) {
	last := reportPollError("topology", nil, "")
	if last != "" {
		t.Fatalf("nil-after-nil returned %q, want empty", last)
	}
	last = reportPollError("topology", errFake("boom"), "")
	if last != "boom" {
		t.Fatalf("first error returned %q, want boom", last)
	}
	last = reportPollError("topology", errFake("boom"), "boom")
	if last != "boom" {
		t.Fatalf("repeat error returned %q, want boom (no re-log)", last)
	}
	last = reportPollError("topology", errFake("other"), "boom")
	if last != "other" {
		t.Fatalf("distinct error returned %q, want other", last)
	}
	last = reportPollError("topology", nil, "other")
	if last != "" {
		t.Fatalf("recovery returned %q, want empty", last)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// snapshotPlatform is a service.API whose JobList result the test can mutate
// between polls, so we can assert the aggregate poller publishes on change and
// stays quiet otherwise. Every other read buildSnapshot performs returns an
// empty-but-non-erroring value; methods left unimplemented would panic, which
// is what we want — it pins buildSnapshot's read surface.
type snapshotPlatform struct {
	service.API
	mu       sync.Mutex
	jobs     []api.JobState
	jobCalls int
}

func (p *snapshotPlatform) setJobs(jobs []api.JobState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = jobs
}

func (p *snapshotPlatform) jobListCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobCalls
}

func (p *snapshotPlatform) StackStatus(context.Context) (api.StackStatusResponse, error) {
	return api.StackStatusResponse{}, nil
}
func (p *snapshotPlatform) ServiceList(context.Context, query.Args) ([]api.ServiceState, int, error) {
	return nil, 0, nil
}
func (p *snapshotPlatform) JobList(_ context.Context, _ query.Args) ([]api.JobState, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobCalls++
	return p.jobs, len(p.jobs), nil
}
func (p *snapshotPlatform) SourceList(context.Context, query.Args) ([]api.SourceState, int, error) {
	return nil, 0, nil
}
func (p *snapshotPlatform) WorkspaceList(context.Context, query.Args) ([]api.WorkspaceRef, int, error) {
	return nil, 0, nil
}
func (p *snapshotPlatform) Templates(context.Context, query.Args) ([]api.TemplateDescriptor, int, error) {
	return nil, 0, nil
}
func (p *snapshotPlatform) SecretsList(context.Context, query.Args) ([]api.SecretRef, int, error) {
	return nil, 0, nil
}
func (p *snapshotPlatform) GitOpsTopology(context.Context) (api.GitOpsTopologyResponse, error) {
	return api.GitOpsTopologyResponse{}, nil
}

func recvSnapshot(t *testing.T, ch <-chan *model.StackSnapshot) *model.StackSnapshot {
	t.Helper()
	select {
	case snap, ok := <-ch:
		if !ok {
			t.Fatal("snapshot channel closed before a value arrived")
		}
		return snap
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a snapshot publish")
		return nil
	}
}

func TestEventHubPublishesSnapshotOnChange(t *testing.T) {
	p := &snapshotPlatform{jobs: []api.JobState{{Name: "seed", Runtime: "container"}}}
	h := NewEventHub(p)
	h.SetPollInterval(20 * time.Millisecond)
	defer h.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.SubscribeSnapshot(ctx)
	h.Start()

	first := recvSnapshot(t, ch)
	if first.Health == nil || first.Health.Status != "ok" {
		t.Fatalf("first snapshot health = %#v, want status ok", first.Health)
	}
	if len(first.Jobs) != 1 || first.Jobs[0].Name != "seed" {
		t.Fatalf("first snapshot jobs = %#v, want one job seed", first.Jobs)
	}

	// Unchanged aggregate must not re-publish across several ticks.
	select {
	case snap := <-ch:
		t.Fatalf("unexpected publish with no change: %#v", snap)
	case <-time.After(150 * time.Millisecond):
	}

	// A changed aggregate publishes the new snapshot.
	p.setJobs([]api.JobState{{Name: "seed", Runtime: "container"}, {Name: "added", Runtime: "local"}})
	second := recvSnapshot(t, ch)
	if len(second.Jobs) != 2 {
		t.Fatalf("second snapshot jobs = %#v, want two jobs", second.Jobs)
	}
}

func TestEventHubSnapshotGuardSkipsReadsWithoutSubscribers(t *testing.T) {
	// With no subscriber attached, pollSnapshot must not touch the platform —
	// the hasSubscribers guard holds, matching pollTopology. Several ticks pass
	// with no subscribe, and JobList must never have been called.
	p := &snapshotPlatform{}
	h := NewEventHub(p)
	h.SetPollInterval(20 * time.Millisecond)
	h.Start()
	time.Sleep(120 * time.Millisecond)
	h.Stop()

	if calls := p.jobListCalls(); calls != 0 {
		t.Fatalf("JobList called %d times with no subscriber, want 0", calls)
	}
}
