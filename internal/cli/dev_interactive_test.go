package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/service"
)

// fakeDevPlatform records the runtime actions the controller dispatches. It
// embeds service.API so only the handful of methods the controller uses need
// implementing; any other call would panic on the nil embedded interface.
type fakeDevPlatform struct {
	service.API
	statuses map[string]api.ServiceState
	calls    []string
}

func (f *fakeDevPlatform) StackStatus(context.Context) (api.StackStatusResponse, error) {
	return api.StackStatusResponse{Services: f.statuses}, nil
}

func (f *fakeDevPlatform) ServiceStart(_ context.Context, names []string) error {
	f.calls = append(f.calls, "start:"+strings.Join(names, ","))
	return nil
}

func (f *fakeDevPlatform) ServiceStop(_ context.Context, names []string) error {
	f.calls = append(f.calls, "stop:"+strings.Join(names, ","))
	return nil
}

func (f *fakeDevPlatform) ServiceRestart(_ context.Context, names []string) error {
	f.calls = append(f.calls, "restart:"+strings.Join(names, ","))
	return nil
}

func newTestController(p service.API) *devController {
	return &devController{
		platform: p,
		out:      io.Discard,
		rootCtx:  context.Background(),
	}
}

func TestApplyOneRestartStartsStoppedService(t *testing.T) {
	fake := &fakeDevPlatform{}
	c := newTestController(fake)

	if quit := c.applyOne(context.Background(), actionRestart, devService{Name: "web", Status: "running"}); quit {
		t.Fatal("applyOne restart must not signal quit-all")
	}
	c.applyOne(context.Background(), actionRestart, devService{Name: "db", Status: "exited"})
	c.applyOne(context.Background(), actionRestart, devService{Name: "cache", Status: ""})

	want := []string{"restart:web", "start:db", "start:cache"}
	assertCalls(t, fake.calls, want)
}

func TestApplyOneQuitStopsSingleService(t *testing.T) {
	fake := &fakeDevPlatform{}
	c := newTestController(fake)

	if quit := c.applyOne(context.Background(), actionQuit, devService{Name: "web", Status: "running"}); quit {
		t.Fatal("quitting a single service must not exit the whole session")
	}
	assertCalls(t, fake.calls, []string{"stop:web"})
}

func TestApplyAllRestartPartitionsRunningAndStopped(t *testing.T) {
	fake := &fakeDevPlatform{}
	c := newTestController(fake)

	services := []devService{
		{Name: "web", Status: "running"},
		{Name: "db", Status: "exited"},
		{Name: "api", Status: "running"},
		{Name: "worker", Status: "stopped"},
	}
	if quit := c.applyAll(context.Background(), actionRestart, services); quit {
		t.Fatal("restart-all must not signal quit-all")
	}
	assertCalls(t, fake.calls, []string{"restart:web,api", "start:db,worker"})
}

func TestApplyAllQuitTearsDownAndExits(t *testing.T) {
	fake := &fakeDevPlatform{}
	c := newTestController(fake)
	cancelled := false
	c.cancelStack = func() { cancelled = true }

	quit := c.applyAll(context.Background(), actionQuit, []devService{{Name: "web", Status: "running"}})
	if !quit {
		t.Fatal("quit-all must signal the caller to exit")
	}
	if !cancelled {
		t.Fatal("quit-all must cancel the stack context to tear the backends down")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("quit-all tears down via context cancel, not per-service stops; got %v", fake.calls)
	}
}

func TestApplyBailsWhenSessionTornDown(t *testing.T) {
	fake := &fakeDevPlatform{}
	c := newTestController(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the dev session has already ended

	if c.applyOne(ctx, actionRestart, devService{Name: "web", Status: "exited"}) {
		t.Fatal("applyOne must not signal quit when the session is torn down")
	}
	if c.applyAll(ctx, actionRestart, []devService{{Name: "web", Status: "exited"}}) {
		t.Fatal("applyAll must not signal quit when the session is torn down")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("no service actions should dispatch after teardown; got %v", fake.calls)
	}
}

func TestDevStackHasLocalProcesses(t *testing.T) {
	mixed := &fakeDevPlatform{statuses: map[string]api.ServiceState{
		"web": {Name: "web", Runtime: "container", Status: "running"},
		"db":  {Name: "db", Runtime: "local", Status: "running"},
	}}
	if !devStackHasLocalProcesses(context.Background(), mixed) {
		t.Fatal("a stack with a local-runtime service must report local processes")
	}
	containerOnly := &fakeDevPlatform{statuses: map[string]api.ServiceState{
		"web": {Name: "web", Runtime: "container", Status: "running"},
	}}
	if devStackHasLocalProcesses(context.Background(), containerOnly) {
		t.Fatal("a container-only stack has no local processes")
	}
}

func TestReadSelection(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		max     int
		wantSel int
		wantOK  bool
	}{
		{"single digit", "3\r", 5, 2, true},
		{"multi digit", "12\n", 15, 11, true},
		{"all lowercase", "a", 4, selectAll, true},
		{"all uppercase", "A", 4, selectAll, true},
		{"empty enter cancels", "\r", 5, 0, false},
		{"escape cancels", "\x1b", 5, 0, false},
		{"out of range", "9\r", 5, 0, false},
		{"backspace corrects", "9\x7f2\r", 5, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestController(nil)
			c.keys = make(chan byte, len(tc.input))
			for i := 0; i < len(tc.input); i++ {
				c.keys <- tc.input[i]
			}
			sel, ok := c.readSelection(context.Background(), tc.max)
			if ok != tc.wantOK || (ok && sel != tc.wantSel) {
				t.Fatalf("readSelection(%q, max=%d) = (%d, %v), want (%d, %v)",
					tc.input, tc.max, sel, ok, tc.wantSel, tc.wantOK)
			}
		})
	}
}

func TestListServicesSortedWithState(t *testing.T) {
	fake := &fakeDevPlatform{statuses: map[string]api.ServiceState{
		"web": {Name: "web", Runtime: "container", Status: "running"},
		"db":  {Name: "db", Runtime: "local", Status: "exited"},
	}}
	c := newTestController(fake)

	services, err := c.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	if len(services) != 2 || services[0].Name != "db" || services[1].Name != "web" {
		t.Fatalf("expected services sorted by name [db web], got %+v", services)
	}
	if services[0].running() {
		t.Fatal("db is exited and must not report running")
	}
	if !services[1].running() {
		t.Fatal("web is running and must report running")
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}
