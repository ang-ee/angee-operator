package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

// serviceStopped is the outcome of a service that ended without failing: it was
// stopped by a signal, or is a container that exists without running. It is
// internal to the summary; jobs use the api.Job* values.
const serviceStopped = "stopped"

// stopSignalExits are the exit statuses of a process ended by the signals
// `angee stop`, `angee down` and Ctrl-C deliver (SIGINT, SIGKILL after the
// grace period, SIGTERM). A service ending with one was stopped, not failed.
var stopSignalExits = map[int]bool{130: true, 137: true, 143: true}

// runtimeOutcome classifies one runtime status sample as one of the api.Job*
// values, or serviceStopped. process-compose and docker compose states are both
// understood. A job fails on any non-zero exit, as the runtime's own
// completed-successfully dependency conditions do; a service ended by a stop
// signal (a negative process-compose exit, or 130/137/143) is stopped instead.
func runtimeOutcome(observed runtime.ServiceStatus, job bool) string {
	exit := 0
	if observed.ExitCode != nil {
		exit = *observed.ExitCode
	}
	switch strings.ToLower(strings.TrimSpace(observed.State)) {
	case "":
		return api.JobNeverRun
	case "unknown":
		return api.JobUnknown
	case "disabled", "created", "scheduled":
		// process-compose leaves a process out of a selective `up` as disabled
		// and one waiting for its cron schedule as scheduled, and compose
		// creates a container it has not started. None of them runs on its own
		// accord during a bring-up.
		if job {
			return api.JobNeverRun
		}
		return serviceStopped
	case "pending":
		return api.JobPending
	case "running", "launching", "launched", "restarting", "foreground", "terminating":
		return api.JobRunning
	case "completed", "exited":
		switch {
		case exit == 0:
			return api.JobCompleted
		case job:
			return api.JobFailed
		case exit < 0 || stopSignalExits[exit]:
			return serviceStopped
		}
		return api.JobFailed
	case "error", "dead":
		return api.JobFailed
	case "skipped":
		return api.JobSkipped
	}
	if job {
		return api.JobUnknown
	}
	return serviceStopped
}

// runtimeExitCode returns the exit status a sample reports for a finished run,
// or nil while it has not finished or could not start.
func runtimeExitCode(observed runtime.ServiceStatus) *int {
	switch strings.ToLower(strings.TrimSpace(observed.State)) {
	case "completed", "exited":
		if observed.ExitCode != nil {
			exit := *observed.ExitCode
			return &exit
		}
	}
	return nil
}

// stackOutcomes classifies every declared service and job of stack from the
// merged runtime states.
func stackOutcomes(stack *manifest.Stack, states map[string]runtime.ServiceStatus) map[string]string {
	outcomes := make(map[string]string, len(stack.Services)+len(stack.Jobs))
	for name := range stack.Services {
		outcomes[name] = runtimeOutcome(states[name], false)
	}
	for name := range stack.Jobs {
		outcomes[name] = runtimeOutcome(states[name], true)
	}
	return outcomes
}

// skipReason explains why the runtime skipped name. process-compose skips a
// process only when a dependency condition fails (it reports a process left out
// by configuration as disabled), so the reason names the failed jobs or
// services the declared dependency chain stops at, following dependencies that
// were skipped themselves. When none is visible, for example because the
// dependency was stopped before it became ready, the reason says so generally.
func skipReason(stack *manifest.Stack, states map[string]runtime.ServiceStatus, outcomes map[string]string, name string) string {
	var causes []string
	seen := map[string]bool{name: true}
	var walk func(string)
	walk = func(node string) {
		dependencies := append([]string(nil), stackNodeDependencies(stack, node)...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if seen[dependency] {
				continue
			}
			seen[dependency] = true
			switch outcomes[dependency] {
			case api.JobFailed:
				causes = append(causes, failureDescription(stack, dependency, states[dependency]))
			case api.JobSkipped:
				walk(dependency)
			}
		}
	}
	walk(name)
	if len(causes) == 0 {
		return "a dependency failed or did not become ready"
	}
	return strings.Join(causes, "; ")
}

// failureDescription names a failed job or service and how it failed, such as
// `job deps failed (exit 1)` or `service web could not start`.
func failureDescription(stack *manifest.Stack, name string, observed runtime.ServiceStatus) string {
	kind := "service"
	if _, ok := stack.Jobs[name]; ok {
		kind = "job"
	}
	return kind + " " + name + " " + failureDetail(observed)
}

// couldNotStart is the reason given for a run whose command the runtime could
// not start (process-compose's Error state).
const couldNotStart = "could not start"

// failureDetail describes how a failed run ended: its exit status, or that the
// runtime could not start its command.
func failureDetail(observed runtime.ServiceStatus) string {
	if exit := runtimeExitCode(observed); exit != nil {
		return "failed (exit " + strconv.Itoa(*exit) + ")"
	}
	if startFailed(observed) {
		return couldNotStart
	}
	return "failed"
}

// startFailed reports whether the runtime could not start the command at all.
func startFailed(observed runtime.ServiceStatus) bool {
	return strings.EqualFold(strings.TrimSpace(observed.State), "error")
}

// countOutcome adds one entry with outcome to counts.
func countOutcome(counts *api.StatusCounts, outcome string) {
	counts.Total++
	switch outcome {
	case api.JobRunning:
		counts.Running++
	case api.JobCompleted:
		counts.Completed++
	case api.JobFailed:
		counts.Failed++
	case api.JobSkipped:
		counts.Skipped++
	}
}

// StackFailureError reports what a dev bring-up left broken: the jobs that
// failed and the services and jobs the runtime skipped because a dependency
// failed.
type StackFailureError struct {
	Failed  []api.JobState
	Skipped []string
}

func (e *StackFailureError) Error() string {
	var parts []string
	for _, job := range e.Failed {
		detail := "failed"
		switch {
		case job.ExitCode != nil:
			detail = "failed (exit " + strconv.Itoa(*job.ExitCode) + ")"
		case job.Reason != "":
			detail = job.Reason
		}
		parts = append(parts, fmt.Sprintf("job %q %s", job.Name, detail))
	}
	if len(e.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: %s", len(e.Skipped), strings.Join(e.Skipped, ", ")))
	}
	hint := "see `angee service list` and `angee job list`"
	if len(e.Failed) == 1 {
		hint = "see `angee job logs " + e.Failed[0].Name + "`"
	} else if len(e.Failed) > 1 {
		hint = "see `angee job logs <name>`"
	}
	return strings.Join(parts, "; ") + "; " + hint
}

// CheckDevJobs waits for the jobs a detached `angee dev` started to finish and
// returns a *StackFailureError when one failed or the runtime skipped a service
// or job. The wait is bounded by ANGEE_JOB_TIMEOUT, the bound of `angee job
// run`; when it elapses first, the error names the unfinished jobs along with
// any failure already visible. progress receives one line naming the jobs it
// waits for.
func CheckDevJobs(ctx context.Context, stack StackStatusReader, progress io.Writer) error {
	status, err := awaitJobs(ctx, stack, jobRunTimeout(), progress)
	failure := stackFailure(status)
	if errors.Is(err, context.Canceled) {
		err = errors.New("stopped waiting for jobs; the stack keeps running")
	}
	if err != nil {
		return errors.Join(err, failure)
	}
	return failure
}

// StackStatusReader reads a stack's status; both API implementations satisfy
// it.
type StackStatusReader interface {
	StackStatus(ctx context.Context) (api.StackStatusResponse, error)
}

// stackFailure returns a *StackFailureError when status shows a failed job or a
// skipped service or job, and nil otherwise.
func stackFailure(status api.StackStatusResponse) error {
	failure := &StackFailureError{}
	for _, name := range sortedKeys(status.Jobs) {
		switch job := status.Jobs[name]; job.Status {
		case api.JobFailed:
			failure.Failed = append(failure.Failed, job)
		case api.JobSkipped:
			failure.Skipped = append(failure.Skipped, name)
		}
	}
	for _, name := range sortedKeys(status.Services) {
		if strings.EqualFold(status.Services[name].Status, api.JobSkipped) {
			failure.Skipped = append(failure.Skipped, name)
		}
	}
	if len(failure.Failed) == 0 && len(failure.Skipped) == 0 {
		return nil
	}
	sort.Strings(failure.Skipped)
	return failure
}

// awaitJobsInterval spaces the status polls of awaitJobs.
var awaitJobsInterval = time.Second

// awaitJobsUnknownPolls is how many polls in a row awaitJobs waits out a job
// whose runtime could not be queried before it counts the job as finished, so
// one failed query does not end the wait early and a backend that stays
// unreachable does not hold it open.
const awaitJobsUnknownPolls = 3

// awaitJobs polls the stack status until no job is pending or running and
// returns the last status read. A job the runtime holds no run for counts as
// finished, so a supervisor that stops does not keep the wait open. The first
// time jobs are unfinished, it writes their names to progress (when not nil).
// A positive timeout bounds the wait; when it elapses first, awaitJobs returns
// the last status with an error naming the unfinished jobs. It returns ctx's
// error when ctx ends.
func awaitJobs(ctx context.Context, stack StackStatusReader, timeout time.Duration, progress io.Writer) (api.StackStatusResponse, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(awaitJobsInterval)
	defer ticker.Stop()
	announced := false
	unknownPolls := 0
	for {
		status, err := stack.StackStatus(ctx)
		if err != nil {
			return api.StackStatusResponse{}, err
		}
		// A status read while ctx ends reflects the cancelled queries, not the
		// jobs.
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		unfinished, unknown := unfinishedJobs(status)
		if unknown {
			unknownPolls++
		} else {
			unknownPolls = 0
		}
		if len(unfinished) == 0 && (!unknown || unknownPolls >= awaitJobsUnknownPolls) {
			return status, nil
		}
		if len(unfinished) > 0 && !announced && progress != nil {
			fmt.Fprintf(progress, "waiting for jobs to finish: %s (Ctrl-C stops waiting; the stack keeps running)\n", strings.Join(unfinished, ", "))
			announced = true
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-deadline:
			if len(unfinished) == 0 {
				return status, nil
			}
			return status, fmt.Errorf("jobs not finished after %s: %s; the stack keeps running (raise %s, or set it to 0, to wait longer)", timeout, strings.Join(unfinished, ", "), jobRunTimeoutEnv)
		case <-ticker.C:
		}
	}
}

// unfinishedJobs returns the sorted names of the jobs status shows pending or
// running, and whether any job's runtime could not be queried.
func unfinishedJobs(status api.StackStatusResponse) ([]string, bool) {
	var names []string
	unknown := false
	for _, name := range sortedKeys(status.Jobs) {
		switch status.Jobs[name].Status {
		case api.JobPending, api.JobRunning:
			names = append(names, name)
		case api.JobUnknown:
			unknown = true
		}
	}
	return names, unknown
}
