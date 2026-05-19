package gql

import (
	"context"
	"sync"
)

// broker is a small in-process fan-out for typed events. Subscribers get a
// buffered receive-only channel; publishes that would block a slow subscriber
// are dropped for that subscriber rather than blocking the producer.
//
// The intended use is GraphQL subscriptions where the producer is a
// change-detection ticker and dropping a duplicate snapshot under load is
// acceptable.
type broker[T any] struct {
	mu     sync.Mutex
	subs   map[*subscription[T]]struct{}
	closed bool
	done   chan struct{}
}

type subscription[T any] struct {
	ch chan T
}

func newBroker[T any]() *broker[T] {
	return &broker[T]{
		subs: make(map[*subscription[T]]struct{}),
		done: make(chan struct{}),
	}
}

// Subscribe registers a new subscriber. The returned channel is closed when
// the context is done or when the broker itself is closed. The buffer size
// controls how many publishes can sit unread before drops kick in.
func (b *broker[T]) Subscribe(ctx context.Context, buf int) <-chan T {
	if buf < 1 {
		buf = 1
	}
	sub := &subscription[T]{ch: make(chan T, buf)}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(sub.ch)
		return sub.ch
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	go func() {
		// Wake on whichever fires first — subscriber's ctx, or the broker
		// itself being closed. Either way the registration is dropped and
		// the channel released. If Close already closed the channel, the
		// map lookup will miss and we skip the redundant close.
		select {
		case <-ctx.Done():
		case <-b.done:
		}
		b.mu.Lock()
		if _, ok := b.subs[sub]; ok {
			delete(b.subs, sub)
			close(sub.ch)
		}
		b.mu.Unlock()
	}()
	return sub.ch
}

// Publish fans v out to every current subscriber, dropping for any whose
// buffer is full. Returns the number of successful deliveries.
func (b *broker[T]) Publish(v T) int {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0
	}
	targets := make([]*subscription[T], 0, len(b.subs))
	for sub := range b.subs {
		targets = append(targets, sub)
	}
	b.mu.Unlock()
	delivered := 0
	for _, sub := range targets {
		select {
		case sub.ch <- v:
			delivered++
		default:
		}
	}
	return delivered
}

// Close releases all subscriber channels and wakes every pending Subscribe
// goroutine. Subsequent Publishes and Subscribes are no-ops; the broker
// cannot be reopened.
func (b *broker[T]) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.done)
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()
	for sub := range subs {
		close(sub.ch)
	}
}

func (b *broker[T]) hasSubscribers() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs) > 0
}
