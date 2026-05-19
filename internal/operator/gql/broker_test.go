package gql

import (
	"context"
	"testing"
	"time"
)

func TestBrokerPublishDelivers(t *testing.T) {
	b := newBroker[int]()
	t.Cleanup(b.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := b.Subscribe(ctx, 4)

	delivered := b.Publish(42)
	if delivered != 1 {
		t.Fatalf("Publish delivered = %d, want 1", delivered)
	}

	select {
	case v := <-ch:
		if v != 42 {
			t.Fatalf("received %d, want 42", v)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive published value")
	}
}

func TestBrokerDropsOnSlowSubscriber(t *testing.T) {
	b := newBroker[int]()
	t.Cleanup(b.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := b.Subscribe(ctx, 1)

	if got := b.Publish(1); got != 1 {
		t.Fatalf("first Publish delivered = %d, want 1", got)
	}
	// Buffer is full; subsequent publishes must drop without blocking.
	if got := b.Publish(2); got != 0 {
		t.Fatalf("second Publish delivered = %d, want 0 (dropped)", got)
	}
	if v := <-ch; v != 1 {
		t.Fatalf("first received = %d, want 1", v)
	}
}

func TestBrokerCtxCancelAutoUnsubscribes(t *testing.T) {
	b := newBroker[int]()
	t.Cleanup(b.Close)

	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx, 1)
	cancel()

	// Wait for the unsubscribe goroutine to clean up.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !b.hasSubscribers() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b.hasSubscribers() {
		t.Fatal("subscriber not removed after ctx cancel")
	}
	// Channel must close so range/receive observers terminate.
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after ctx cancel")
	}
}

func TestBrokerCloseReleasesSubscribers(t *testing.T) {
	b := newBroker[int]()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := b.Subscribe(ctx, 1)

	b.Close()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel should close on broker Close")
	}
	// Subsequent Publish is a no-op and must not panic.
	if delivered := b.Publish(1); delivered != 0 {
		t.Fatalf("post-close Publish delivered = %d, want 0", delivered)
	}
	// Idempotent Close.
	b.Close()
}

func TestBrokerCloseWakesBackgroundSubscriber(t *testing.T) {
	// Subscribers that pass context.Background() must not leak their
	// auto-unsubscribe goroutine when the broker is Closed before any
	// natural ctx cancellation.
	b := newBroker[int]()
	ch := b.Subscribe(context.Background(), 1)

	doneClosed := make(chan struct{})
	go func() {
		// The receive returns ok=false once the channel is closed by Close().
		<-ch
		close(doneClosed)
	}()
	b.Close()

	select {
	case <-doneClosed:
	case <-time.After(time.Second):
		t.Fatal("subscriber goroutine not woken by Close")
	}
}

func TestBrokerSubscribeAfterCloseReturnsClosedChan(t *testing.T) {
	b := newBroker[int]()
	b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := b.Subscribe(ctx, 1)
	if _, ok := <-ch; ok {
		t.Fatal("subscribe after Close should return a pre-closed channel")
	}
}
