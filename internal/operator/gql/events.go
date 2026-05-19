package gql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
)

// errSubscriptionsUnavailable is returned when a subscription resolver fires
// against a Resolver that was constructed without an EventHub.
var errSubscriptionsUnavailable = errors.New("subscriptions are not enabled on this operator")

const (
	defaultEventPollInterval = 2 * time.Second
	subscriberBuffer         = 4
)

// EventHub powers GraphQL subscriptions by polling the platform for snapshot
// changes (topology, per-workspace status) and fanning the resulting changed
// snapshots out to subscribers. Log subscriptions ride the platform's
// existing follow channels directly and don't pass through a broker.
//
// The hub is safe for concurrent use. It is owned by the operator Server,
// started before the first subscriber connects and stopped on server
// shutdown.
type EventHub struct {
	platform     *service.Platform
	pollInterval time.Duration

	topology *broker[*api.GitOpsTopologyResponse]

	mu               sync.Mutex
	workspaceBrokers map[string]*broker[*api.WorkspaceStatusResponse]
	stopped          bool

	startOnce sync.Once
	cancel    context.CancelFunc
	rootCtx   context.Context
	wg        sync.WaitGroup
}

// NewEventHub constructs a hub bound to the given platform. The caller must
// invoke Start before the first subscriber connects and Stop on shutdown.
func NewEventHub(p *service.Platform) *EventHub {
	return &EventHub{
		platform:         p,
		pollInterval:     defaultEventPollInterval,
		topology:         newBroker[*api.GitOpsTopologyResponse](),
		workspaceBrokers: make(map[string]*broker[*api.WorkspaceStatusResponse]),
	}
}

// SetPollInterval overrides the default tick used by change-detection
// goroutines. Tests use a shorter interval; production should leave the
// default in place.
func (h *EventHub) SetPollInterval(d time.Duration) {
	if d > 0 {
		h.pollInterval = d
	}
}

// Start launches the topology poller. Per-workspace status pollers are
// started lazily on first subscribe. Idempotent: subsequent calls are
// no-ops, even across goroutines.
func (h *EventHub) Start() {
	h.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		h.rootCtx = ctx
		h.cancel = cancel
		h.wg.Add(1)
		go h.pollTopology(ctx)
	})
}

// Stop tears down all background pollers and closes every subscriber channel.
// Safe to call multiple times; safe to call before Start. After Stop, new
// subscribes return a pre-closed channel.
func (h *EventHub) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	cancel := h.cancel
	h.cancel = nil
	h.rootCtx = nil
	h.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	h.wg.Wait()

	h.topology.Close()
	h.mu.Lock()
	for _, b := range h.workspaceBrokers {
		b.Close()
	}
	h.workspaceBrokers = map[string]*broker[*api.WorkspaceStatusResponse]{}
	h.mu.Unlock()
}

func (h *EventHub) pollTopology(ctx context.Context) {
	defer h.wg.Done()
	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()
	var (
		last    [32]byte
		lastErr string
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !h.topology.hasSubscribers() {
			continue
		}
		topo, err := h.platform.GitOpsTopology(ctx)
		lastErr = reportPollError("topology", err, lastErr)
		if err != nil {
			continue
		}
		next, ok := hashJSON(topo)
		if !ok || next == last {
			continue
		}
		last = next
		h.topology.Publish(&topo)
	}
}

func (h *EventHub) pollWorkspaceStatus(ctx context.Context, name string, b *broker[*api.WorkspaceStatusResponse]) {
	defer h.wg.Done()
	ticker := time.NewTicker(h.pollInterval)
	defer ticker.Stop()
	prefix := fmt.Sprintf("workspace[%s] status", name)
	var (
		last    [32]byte
		lastErr string
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !b.hasSubscribers() {
			continue
		}
		status, err := h.platform.WorkspaceStatus(ctx, name)
		lastErr = reportPollError(prefix, err, lastErr)
		if err != nil {
			continue
		}
		next, ok := hashJSON(status)
		if !ok || next == last {
			continue
		}
		last = next
		b.Publish(&status)
	}
}

// reportPollError logs the first occurrence of each distinct polling error
// (and a recovery line when it clears), so a misconfigured stack doesn't
// silently drop events. Returns the updated lastErr to thread through the
// caller's loop.
func reportPollError(prefix string, err error, lastErr string) string {
	if err == nil {
		if lastErr != "" {
			fmt.Fprintf(os.Stderr, "operator: %s poll recovered\n", prefix)
		}
		return ""
	}
	if msg := err.Error(); msg != lastErr {
		fmt.Fprintf(os.Stderr, "operator: %s poll failed: %v\n", prefix, err)
		return msg
	}
	return lastErr
}

// SubscribeTopology returns a channel that receives a new topology snapshot
// every time the polled result changes. The channel closes when ctx is done
// or when the hub stops. Subscribers do NOT receive an initial snapshot —
// issue a `gitOpsTopology` query alongside the subscription if you need
// the current state on connect.
func (h *EventHub) SubscribeTopology(ctx context.Context) <-chan *api.GitOpsTopologyResponse {
	return h.topology.Subscribe(ctx, subscriberBuffer)
}

// SubscribeWorkspaceStatus subscribes to status snapshot changes for one
// named workspace. The per-workspace poller is started on first subscribe
// and runs until the hub stops; later subscribers re-attach to the same
// ticker. Subscribes that arrive after Stop return a pre-closed channel.
func (h *EventHub) SubscribeWorkspaceStatus(ctx context.Context, name string) <-chan *api.WorkspaceStatusResponse {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		closed := make(chan *api.WorkspaceStatusResponse)
		close(closed)
		return closed
	}
	b, ok := h.workspaceBrokers[name]
	if !ok {
		b = newBroker[*api.WorkspaceStatusResponse]()
		h.workspaceBrokers[name] = b
		if h.rootCtx != nil {
			h.wg.Add(1)
			go h.pollWorkspaceStatus(h.rootCtx, name, b)
		}
	}
	h.mu.Unlock()
	return b.Subscribe(ctx, subscriberBuffer)
}

// SubscribeServiceLogs returns a follow-tail channel for the named service.
// The channel is owned by the runtime backend; cancelling ctx tears down
// the underlying `logs --follow` process.
func (h *EventHub) SubscribeServiceLogs(ctx context.Context, name string) (<-chan string, error) {
	return h.platform.StackLogs(ctx, []string{name}, true)
}

// SubscribeWorkspaceLogs returns a follow-tail channel for the named
// workspace. Same lifecycle semantics as SubscribeServiceLogs.
func (h *EventHub) SubscribeWorkspaceLogs(ctx context.Context, name string) (<-chan string, error) {
	return h.platform.WorkspaceLogs(ctx, name, true)
}

func hashJSON(v any) ([32]byte, bool) {
	data, err := json.Marshal(v)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(data), true
}
