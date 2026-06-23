package gql

import (
	"context"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/query"
)

// liveSubInterval is the poll cadence backing the Hasura-style live-list
// subscriptions (the operator has no change feed; it polls service.API).
const liveSubInterval = 2 * time.Second

// This file holds the Hasura-dialect glue: single-row lookups backing *_by_pk,
// the live-list subscription poller, plus the where/order_by binders onto the
// internal/query engine (below).

// serviceByID / jobByID back the *_by_pk resolvers for the two entities that
// have no dedicated single-item service.API getter.
func (r *Resolver) serviceByID(ctx context.Context, id string) (*api.ServiceState, error) {
	items, _, err := r.Platform.ServiceList(ctx, query.Args{})
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Name == id {
			return &items[i], nil
		}
	}
	return nil, nil
}

func (r *Resolver) jobByID(ctx context.Context, id string) (*api.JobState, error) {
	items, _, err := r.Platform.JobList(ctx, query.Args{})
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Name == id {
			return &items[i], nil
		}
	}
	return nil, nil
}

// liveList powers a Hasura-style per-table live subscription: it polls fetch on
// an interval and emits the current page whenever its content hash changes,
// closing when the subscription context is cancelled. One goroutine per
// subscriber — adequate for the operator's small, in-memory collections.
func liveList[T any](ctx context.Context, interval time.Duration, fetch func(context.Context) ([]*T, error)) <-chan []*T {
	ch := make(chan []*T, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var last [32]byte
		var have bool
		emit := func() bool {
			items, err := fetch(ctx)
			if err != nil {
				return true // transient; keep polling
			}
			if h, ok := hashJSON(items); ok {
				if have && h == last {
					return true
				}
				last, have = h, true
			}
			select {
			case ch <- items:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !emit() {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !emit() {
					return
				}
			}
		}
	}()
	return ch
}
