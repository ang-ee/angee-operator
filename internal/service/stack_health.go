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

// stopSignalExits are the exit statuses of a process ended by the signals
// `angee stop`, `angee down` and Ctrl-C deliver (SIGINT, SIGKILL after the
// grace period, SIGTERM). A service ending with one was stopped, not failed.
var stopSignalExits = map[int]bool{130: true, 137: true, 143: true}

// runtimeOutcome classifies one runtime status sample as one of the api.Job*
// values, or api.ServiceStopped. process-compose and docker compose states are
// both understood. A job fails on any non-zero exit, as the runtime's own
// completed-successfully dependency conditions do; a service ended by a stop
// signal (a negative process-compose exit, or 130/137/143) is stopped instead.
// process-compose reports -1 for any signal, so a local service a signal
// crashed or killed reads as stopped too.
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
		return api.ServiceStopped
	case "pending":
		return api.JobPending
	case "restarting":
		// The run ended and the runtime's restart policy starts it again after
		// its backoff. Only services have a restart policy.
		if job {
			return api.JobRunning
		}
		return api.ServiceRestarting
	case "running", "launching", "launched", "foreground", "terminating":
		return api.JobRunning
	case "completed", "exited":
		switch {
		case exit == 0:
			return api.JobCompleted
		case job:
			return api.JobFailed
		case exit < 0 || stopSignalExits[exit]:
			return api.ServiceStopped
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
	return api.ServiceStopped
}

// runtimeExitCode returns the exit status a sample reports for a finished run,
// including the run a restarting service just ended, or nil while it has not
// finished or could not start.
func runtimeExitCode(observed runtime.ServiceStatus) *int {
	switch strings.ToLower(strings.TrimSpace(observed.State)) {
	case "completed", "exited", "restarting":
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

// waitReason explains why a pending service or job is still waiting when the
// cause is a dependency the runtime keeps restarting, such as `waiting for
// service db: restarting (exit 1, 3 restarts)`, and is empty otherwise.
// process-compose starts a dependent of a service with a readiness probe only
// once that service is healthy, so a dependent of one in a crash loop stays
// pending for as long as the loop lasts. The walk follows dependencies that are
// pending themselves.
func waitReason(stack *manifest.Stack, states map[string]runtime.ServiceStatus, outcomes map[string]string, name string) string {
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
			if service, ok := stack.Services[dependency]; ok {
				if detail := restartDetail(states[dependency], outcomes[dependency], service.Ready != nil); detail != "" {
					causes = append(causes, "service "+dependency+": "+detail)
					continue
				}
			}
			if outcomes[dependency] == api.JobPending {
				walk(dependency)
			}
		}
	}
	walk(name)
	if len(causes) == 0 {
		return ""
	}
	return "waiting for " + strings.Join(causes, "; ")
}

// restartDetail describes a service the runtime keeps restarting: one waiting
// out its restart backoff (`restarting (exit 1, 3 restarts)`), or one with a
// readiness probe (probed) that it has restarted and that runs without being
// healthy (`restarted 3 times, not healthy`). Only a probed service holds its
// dependents back while it runs, and one without a probe never reports
// healthy, so it is never named for running. It is empty for any other
// service.
func restartDetail(observed runtime.ServiceStatus, outcome string, probed bool) string {
	switch {
	case outcome == api.ServiceRestarting:
		return RestartingText(runtimeExitCode(observed), observed.Restarts)
	case outcome == api.JobRunning && probed && observed.Restarts > 0 && observed.Health != "healthy":
		return fmt.Sprintf("restarted %d time(s), not healthy", observed.Restarts)
	}
	return ""
}

// RestartingText describes a restarting service from the exit that ended its
// last run and its restart count, such as `restarting (exit 1, 3 restarts)`.
func RestartingText(exit *int, restarts int) string {
	var facts []string
	if exit != nil {
		facts = append(facts, "exit "+strconv.Itoa(*exit))
	}
	switch {
	case restarts == 1:
		facts = append(facts, "1 restart")
	case restarts > 1:
		facts = append(facts, strconv.Itoa(restarts)+" restarts")
	}
	if len(facts) == 0 {
		return api.ServiceRestarting
	}
	return api.ServiceRestarting + " (" + strings.Join(facts, ", ") + ")"
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
	case api.ServiceRestarting:
		counts.Restarting++
	case api.JobSkipped:
		counts.Skipped++
	}
}

// StackFailureError reports what a dev bring-up left broken: the jobs that
// failed, the services the runtime is restarting after they exited, and the
// services and jobs the runtime skipped because a dependency failed.
type StackFailureError struct {
	Failed     []api.JobState
	Restarting []api.ServiceState
	Skipped    []string
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
	for _, service := range e.Restarting {
		parts = append(parts, fmt.Sprintf("service %q %s", service.Name, RestartingText(service.ExitCode, service.Restarts)))
	}
	if len(e.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped: %s", len(e.Skipped), strings.Join(e.Skipped, ", ")))
	}
	hint := "see `angee service list` and `angee job list`"
	switch {
	case len(e.Failed) == 1 && len(e.Restarting) == 0:
		hint = "see `angee job logs " + e.Failed[0].Name + "`"
	case len(e.Failed) > 1 && len(e.Restarting) == 0:
		hint = "see `angee job logs <name>`"
	case len(e.Failed) == 0 && len(e.Restarting) == 1:
		hint = "see `angee service logs " + e.Restarting[0].Name + "`"
	}
	return strings.Join(parts, "; ") + "; " + hint
}

// CheckDevJobs waits for the jobs a detached `angee dev` started to finish and
// returns a *StackFailureError when one failed, the runtime is restarting a
// service, or the runtime skipped a service or job. The wait is bounded by
// ANGEE_JOB_TIMEOUT, the bound of `angee job run`; when it elapses first, the
// error names the unfinished jobs along with any failure already visible.
// progress receives one line naming the jobs it waits for, and one for each
// job found waiting on a service the runtime keeps restarting.
func CheckDevJobs(ctx context.Context, stack StackStatusReader, progress io.Writer) error {
	status, err := awaitJobs(ctx, stack, jobRunTimeout(), progress, func(job, reason string) {
		if progress != nil {
			fmt.Fprintf(progress, "job %s is %s\n", job, reason)
		}
	})
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

// stackFailure returns a *StackFailureError when status shows a failed job, a
// service the runtime is restarting, or a skipped service or job, and nil
// otherwise.
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
		service := status.Services[name]
		if strings.EqualFold(service.Status, api.JobSkipped) {
			failure.Skipped = append(failure.Skipped, name)
		}
		if service.Outcome == api.ServiceRestarting {
			failure.Restarting = append(failure.Restarting, service)
		}
	}
	if len(failure.Failed) == 0 && len(failure.Restarting) == 0 && len(failure.Skipped) == 0 {
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
// The first time a pending job's reason names a service the runtime keeps
// restarting, it passes the job and reason to reportWait (when not nil), from
// the status reads it makes anyway.
// A positive timeout bounds the wait; when it elapses first, awaitJobs returns
// the last status with an error naming the unfinished jobs. It returns ctx's
// error when ctx ends.
func awaitJobs(ctx context.Context, stack StackStatusReader, timeout time.Duration, progress io.Writer, reportWait func(job, reason string)) (api.StackStatusResponse, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(awaitJobsInterval)
	defer ticker.Stop()
	announced := false
	reported := map[string]bool{}
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
		for _, name := range unfinished {
			job := status.Jobs[name]
			if job.Status != api.JobPending || job.Reason == "" || reported[name] || reportWait == nil {
				continue
			}
			reportWait(name, job.Reason)
			reported[name] = true
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
