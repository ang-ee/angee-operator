//go:build !windows

// Package processgroup configures an os/exec command for bounded, whole-group
// cancellation on operating systems with Unix process groups.
package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var signalGroup = func(pid int, signal syscall.Signal) error {
	return syscall.Kill(-pid, signal)
}

type stoppableTimer interface {
	Stop() bool
}

var afterGroupDelay = func(delay time.Duration, f func()) stoppableTimer {
	return time.AfterFunc(delay, f)
}

// Configure isolates cmd in a process group. Context cancellation interrupts
// the whole group, then kills surviving members after waitDelay. The returned
// function must be called after cmd.Wait returns. It cancels the delayed kill
// when the group is already gone, or kills residual members left after either
// cancellation or a normally exiting group leader.
func Configure(cmd *exec.Cmd, waitDelay time.Duration) (func(), error) {
	return configure(cmd, waitDelay, false)
}

// ConfigureImmediateKill isolates cmd in a process group and kills the whole
// group immediately on context cancellation. Its completion function performs
// the same one-shot post-Wait survivor cleanup as Configure.
func ConfigureImmediateKill(cmd *exec.Cmd) (func(), error) {
	return configure(cmd, 0, true)
}

func configure(cmd *exec.Cmd, waitDelay time.Duration, immediate bool) (func(), error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay

	var (
		mu           sync.Mutex
		timer        stoppableTimer
		canceled     bool
		completed    bool
		killDone     = make(chan struct{})
		completeOnce sync.Once
	)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pid := cmd.Process.Pid
		mu.Lock()
		if completed {
			mu.Unlock()
			return os.ErrProcessDone
		}
		if immediate {
			canceled = true
			mu.Unlock()
			err := signalGroup(pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		if !canceled {
			canceled = true
			timer = afterGroupDelay(waitDelay, func() {
				mu.Lock()
				defer mu.Unlock()
				_ = signalGroup(pid, syscall.SIGKILL)
				close(killDone)
			})
		}
		mu.Unlock()
		err := signalGroup(pid, syscall.SIGINT)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return func() {
		completeOnce.Do(func() {
			if cmd.Process == nil {
				return
			}
			pid := cmd.Process.Pid
			mu.Lock()
			completed = true
			wasCanceled := canceled
			if wasCanceled && !immediate {
				aliveErr := signalGroup(pid, 0)
				if errors.Is(aliveErr, syscall.ESRCH) && timer.Stop() {
					close(killDone)
				}
				done := killDone
				mu.Unlock()
				<-done
				return
			}
			mu.Unlock()
			if errors.Is(signalGroup(pid, 0), syscall.ESRCH) {
				return
			}
			_ = signalGroup(pid, syscall.SIGKILL)
		})
	}, nil
}
