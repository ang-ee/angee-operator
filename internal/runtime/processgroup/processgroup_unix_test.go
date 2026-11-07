//go:build !windows

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeStoppableTimer struct{}

func (fakeStoppableTimer) Stop() bool { return false }

func TestConfigureSignalsTheWholeGroup(t *testing.T) {
	original := signalGroup
	t.Cleanup(func() { signalGroup = original })

	var (
		mu      sync.Mutex
		signals []struct {
			pid    int
			signal syscall.Signal
		}
	)
	killed := make(chan struct{})
	signalGroup = func(pid int, signal syscall.Signal) error {
		mu.Lock()
		signals = append(signals, struct {
			pid    int
			signal syscall.Signal
		}{pid: pid, signal: signal})
		mu.Unlock()
		if signal == syscall.SIGKILL {
			close(killed)
		}
		return nil
	}

	cmd := exec.CommandContext(t.Context(), "unused")
	cmd.Process = &os.Process{Pid: 42}
	stop, err := Configure(cmd, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := cmd.Cancel(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("group was not killed after the wait delay")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signals) != 2 || signals[0].pid != 42 || signals[0].signal != syscall.SIGINT || signals[1].pid != 42 || signals[1].signal != syscall.SIGKILL {
		t.Fatalf("signals = %+v, want SIGINT then SIGKILL for process group 42", signals)
	}
}

func TestConfigurePreservesTheWaitDelayWhenTheLeaderExitsFirst(t *testing.T) {
	originalSignalGroup := signalGroup
	originalAfterGroupDelay := afterGroupDelay
	t.Cleanup(func() {
		signalGroup = originalSignalGroup
		afterGroupDelay = originalAfterGroupDelay
	})

	checked := make(chan struct{})
	killed := make(chan struct{})
	var checkedOnce sync.Once
	signalGroup = func(_ int, signal syscall.Signal) error {
		if signal == 0 {
			checkedOnce.Do(func() { close(checked) })
		}
		if signal == syscall.SIGKILL {
			close(killed)
		}
		return nil
	}
	var fireDelayedKill func()
	afterGroupDelay = func(_ time.Duration, f func()) stoppableTimer {
		fireDelayedKill = f
		return fakeStoppableTimer{}
	}
	cmd := exec.CommandContext(t.Context(), "unused")
	cmd.Process = &os.Process{Pid: 42}
	stop, err := Configure(cmd, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatal(err)
	}
	if fireDelayedKill == nil {
		t.Fatal("cancel did not schedule the delayed group kill")
	}
	completed := make(chan struct{})
	go func() {
		stop()
		close(completed)
	}()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("completion did not check for residual group members")
	}
	select {
	case <-completed:
		t.Fatal("completion did not preserve the wait delay")
	default:
	}
	fireDelayedKill()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("completion did not wait for the delayed group kill")
	}
	select {
	case <-killed:
	default:
		t.Fatal("group was not killed when the wait delay elapsed")
	}
}

func TestConfigureDoesNotKillAnAlreadyGoneGroupAtCompletion(t *testing.T) {
	original := signalGroup
	t.Cleanup(func() { signalGroup = original })

	var signals []syscall.Signal
	signalGroup = func(_ int, signal syscall.Signal) error {
		signals = append(signals, signal)
		return syscall.ESRCH
	}
	cmd := exec.CommandContext(t.Context(), "unused")
	cmd.Process = &os.Process{Pid: 42}
	stop, err := Configure(cmd, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel() after completion = %v, want os.ErrProcessDone", err)
	}
	if len(signals) != 1 || signals[0] != 0 {
		t.Fatalf("signals = %v, want only the liveness check and no signal to the gone group", signals)
	}
}

func TestConfigureImmediateKillKillsTheWholeGroupOnCancel(t *testing.T) {
	original := signalGroup
	t.Cleanup(func() { signalGroup = original })

	var signals []syscall.Signal
	signalGroup = func(_ int, signal syscall.Signal) error {
		signals = append(signals, signal)
		if signal == 0 {
			return syscall.ESRCH
		}
		return nil
	}
	cmd := exec.CommandContext(t.Context(), "unused")
	cmd.Process = &os.Process{Pid: 42}
	stop, err := ConfigureImmediateKill(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatal(err)
	}
	stop()
	if len(signals) != 2 || signals[0] != syscall.SIGKILL || signals[1] != 0 {
		t.Fatalf("signals = %v, want immediate SIGKILL then the completion liveness check", signals)
	}
}
