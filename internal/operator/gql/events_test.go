package gql

import (
	"context"
	"testing"
	"time"
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
