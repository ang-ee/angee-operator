package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/runtime"
	"github.com/ang-ee/angee-operator/internal/runtime/compose"
	"github.com/ang-ee/angee-operator/internal/runtime/edge"
	"github.com/ang-ee/angee-operator/internal/runtime/proccompose"
)

func (p *Platform) JobList(ctx context.Context, q query.Args) ([]api.JobState, int, error) {
	if err := query.Validate(q, queryfields.Job); err != nil {
		return nil, 0, invalidQueryError(err)
	}
	status, err := p.StackStatus(ctx)
	if err != nil {
		return nil, 0, err
	}
	jobs := make([]api.JobState, 0, len(status.Jobs))
	for _, name := range sortedKeys(status.Jobs) {
		jobs = append(jobs, status.Jobs[name])
	}
	page, total := query.Apply(jobs, q, queryfields.Job)
	return page, total, nil
}

var jobRunSequence atomic.Uint64

// CaptureJobError refuses a backup capture job at a job entry point. A capture
// job runs only inside a backup (BackupStart), which chooses its output paths
// and is reachable only with the operator admin bearer; run as an ordinary job,
// any caller could choose where it writes the stack's data.
type CaptureJobError struct{ Job string }

func (e *CaptureJobError) Error() string {
	return fmt.Sprintf("job %q is a backup capture job and runs only as part of a backup", e.Job)
}

// refuseCaptureJob is the INVALID_INPUT a job entry point returns for a capture
// job, and nil for any other name, declared or not.
func refuseCaptureJob(stack *manifest.Stack, name string) error {
	if job, ok := stack.Jobs[name]; !ok || job.Capture == nil {
		return nil
	}
	refused := &CaptureJobError{Job: name}
	hint := "Select its capture in a backup (POST /backups) instead."
	return &OperationError{Code: CodeInvalidInput, Job: name, Hint: hint, Summary: refused.Error() + ". " + hint, Err: refused}
}

func (p *Platform) JobRunStart(ctx context.Context, name string, inputs map[string]string, chainedRestart bool) (receipt api.JobRunOperation, err error) {
	defer jobOperation(name, chainedRestart).annotate(&err)
	// Refuse a capture job before taking the lease or preparing anything; the
	// stack this run executes is checked again once it is loaded below.
	declared, err := p.LoadStack()
	if err != nil {
		return api.JobRunOperation{}, err
	}
	if err := refuseCaptureJob(declared, name); err != nil {
		return api.JobRunOperation{}, err
	}
	ctx, release, err := p.beginMutation(ctx, "job")
	if err != nil {
		return api.JobRunOperation{}, err
	}
	jobInputs := map[string]map[string]string(nil)
	if len(inputs) != 0 {
		jobInputs = map[string]map[string]string{name: inputs}
	}
	compiled, err := p.stackPrepare(ctx, jobInputs, false, name, chainedRestart, true)
	if err != nil {
		release()
		return api.JobRunOperation{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		release()
		return api.JobRunOperation{}, err
	}
	if _, ok := stack.Jobs[name]; !ok {
		release()
		return api.JobRunOperation{}, &NotFoundError{Kind: "job", Name: name}
	}
	if err := refuseCaptureJob(stack, name); err != nil {
		release()
		return api.JobRunOperation{}, err
	}
	p.jobRunsMu.Lock()
	if p.activeJobRun {
		p.jobRunsMu.Unlock()
		release()
		return api.JobRunOperation{}, mutationBusyError("another job operation is active")
	}
	p.activeJobRun = true
	id := strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + strconv.FormatUint(jobRunSequence.Add(1), 36)
	nodes, err := jobRunNodes(stack, name, chainedRestart)
	if err != nil {
		p.activeJobRun = false
		p.jobRunsMu.Unlock()
		release()
		return api.JobRunOperation{}, err
	}
	op := api.JobRunOperation{ID: id, RootJob: name, ChainedRestart: chainedRestart, Status: api.JobRunPending, StartedAt: time.Now().UTC(), Nodes: nodes}
	p.rememberJobRun(op, stack, compiled)
	// Snapshot the receipt while still holding the lock. Returning it after the
	// executor goroutine spawns would race the executor's setNode writes, which
	// mutate the stored record's Nodes backing array (WARNING: DATA RACE).
	receipt = cloneJobRun(op)
	p.jobRunsMu.Unlock()
	operationContext := ctx
	if p.detachedContext != nil {
		operationContext = p.detachedContext
	}
	go p.executeJobRun(operationContext, id, release)
	return receipt, nil
}

// JobRun preserves the synchronous local API while the operator transports use
// JobRunStart to obtain a durable receipt immediately.
func (p *Platform) JobRun(ctx context.Context, name string, inputs map[string]string) (output []byte, err error) {
	defer jobOperation(name, false).annotate(&err)
	op, err := p.JobRunStart(ctx, name, inputs, false)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for op.Status == api.JobRunPending || op.Status == api.JobRunRunning {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			op, err = p.JobRunGet(ctx, op.ID)
			if err != nil {
				return nil, err
			}
		}
	}
	if op.Status == api.JobRunFailed {
		return []byte(op.Output), JobRunFailure(op)
	}
	return []byte(op.Output), nil
}

// JobRunFailure is the error JobRun returns for a failed run, built from its
// receipt, so the local platform and a remote operator's client report it the
// same way.
func JobRunFailure(op api.JobRunOperation) error {
	code := op.ErrorCode
	if code == "" {
		code = CodeJobFailed
	}
	run := jobOperation(op.RootJob, op.ChainedRestart)
	return &OperationError{Code: code, Cause: op.ErrorCause, Operation: run.name, Job: op.RootJob, Title: run.title, Summary: op.Error}
}

func (p *Platform) JobRunGet(ctx context.Context, id string) (api.JobRunOperation, error) {
	p.jobRunsMu.RLock()
	op, ok := p.jobRuns[id]
	if ok {
		snapshot := cloneJobRun(op)
		p.jobRunsMu.RUnlock()
		return snapshot, nil
	}
	p.jobRunsMu.RUnlock()
	return p.readJobReceipt(ctx, id, false)
}

func (p *Platform) LatestJobRun(_ context.Context) (*api.JobRunOperation, error) {
	p.jobRunsMu.RLock()
	defer p.jobRunsMu.RUnlock()
	stored, ok := p.jobRuns[p.latestJobRun]
	if !ok {
		return nil, nil
	}
	op := cloneJobRun(stored)
	return &op, nil
}

func cloneJobRun(op api.JobRunOperation) api.JobRunOperation {
	op.Nodes = append([]api.JobRunNode(nil), op.Nodes...)
	if op.Artifact != nil {
		// Artifact evidence includes nested collections; clone through its wire type.
		data, _ := json.Marshal(op.Artifact)
		var artifact api.BackupArtifact
		_ = json.Unmarshal(data, &artifact)
		op.Artifact = &artifact
	}
	return op
}

func (p *Platform) JobRunPreview(_ context.Context, name string, chained bool) (api.JobRunPreview, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return api.JobRunPreview{}, err
	}
	if err := refuseCaptureJob(stack, name); err != nil {
		return api.JobRunPreview{}, err
	}
	nodes, err := jobRunNodes(stack, name, chained)
	if err != nil {
		return api.JobRunPreview{}, err
	}
	var result api.JobRunPreview
	for _, n := range nodes {
		if n.Kind == "job" {
			result.Jobs = append(result.Jobs, n.Name)
		} else {
			result.Services = append(result.Services, n.Name)
		}
	}
	return result, nil
}

func jobRunNodes(stack *manifest.Stack, root string, chained bool) ([]api.JobRunNode, error) {
	if _, ok := stack.Jobs[root]; !ok {
		return nil, &NotFoundError{Kind: "job", Name: root}
	}
	seen := map[string]bool{root: true}
	if chained {
		changed := true
		for changed {
			changed = false
			for name, j := range stack.Jobs {
				// A capture job runs only in a backup, never as a restarted
				// prerequisite's dependent.
				if j.Capture != nil {
					continue
				}
				for _, d := range j.DependsOn {
					if seen[d] && !seen[name] {
						seen[name] = true
						changed = true
					}
				}
			}
			for name, s := range stack.Services {
				for _, d := range append(s.After, s.DependsOn...) {
					if seen[d] && !seen[name] {
						seen[name] = true
						changed = true
					}
				}
			}
		}
	}
	// Holds do not select more jobs. They add service outcomes to the receipt,
	// even for a plain run, and service dependents to a chained recovery.
	held := map[string]bool{}
	for name := range seen {
		for _, service := range stack.Jobs[name].Holds {
			held[service] = true
		}
	}
	for name := range held {
		seen[name] = true
	}
	if chained && len(held) > 0 {
		changed := true
		for changed {
			changed = false
			for name := range stack.Services {
				for _, dependency := range stackNodeDependencies(stack, name) {
					if seen[dependency] && !seen[name] {
						seen[name], changed = true, true
					}
				}
			}
		}
	}
	if len(held) > 0 {
		// A service selected for recovery cannot also be a prerequisite of
		// the jobs that must finish before that recovery. Reject before stops.
		var check func(string, map[string]bool) error
		check = func(name string, visited map[string]bool) error {
			if visited[name] {
				return nil
			}
			visited[name] = true
			for _, dependency := range stackNodeDependencies(stack, name) {
				if _, service := stack.Services[dependency]; service && seen[dependency] {
					return fmt.Errorf("job hold operation requires service %q before its recovery", dependency)
				}
				if err := check(dependency, visited); err != nil {
					return err
				}
			}
			return nil
		}
		for _, name := range sortedKeys(stack.Jobs) {
			if seen[name] {
				if err := check(name, map[string]bool{}); err != nil {
					return nil, err
				}
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	nodes := make([]api.JobRunNode, 0, len(names))
	for _, n := range names {
		kind := "service"
		if _, ok := stack.Jobs[n]; ok {
			kind = "job"
		}
		nodes = append(nodes, api.JobRunNode{Name: n, Kind: kind, Status: api.JobRunPending})
	}
	return nodes, nil
}

// jobOperationStack returns the complete runtime graph needed by one operation:
// its existing run/restart nodes plus every transitive prerequisite. The view is
// compiler-only; stack declarations and generated runtime documents stay whole.
func jobOperationStack(stack *manifest.Stack, root string, chained bool) (*manifest.Stack, error) {
	nodes, err := jobRunNodes(stack, root, chained)
	if err != nil {
		return nil, err
	}
	included := make(map[string]bool, len(nodes))
	queue := make([]string, 0, len(nodes))
	for _, node := range nodes {
		included[node.Name] = true
		queue = append(queue, node.Name)
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		dependencies := append(stackNodeDependencies(stack, name), stack.Jobs[name].Holds...)
		for _, dependency := range dependencies {
			if included[dependency] {
				continue
			}
			if _, isJob := stack.Jobs[dependency]; !isJob {
				if _, isService := stack.Services[dependency]; !isService {
					continue
				}
			}
			included[dependency] = true
			queue = append(queue, dependency)
		}
	}
	projected := *stack
	// The edge is a stack-wide generated service, not an operation prerequisite.
	// Its wiring of the routed services this view keeps is added back from the
	// whole stack after compiling (contributeOperationIngress).
	projected.Ingress = manifest.Ingress{}
	projected.Jobs = make(map[string]manifest.Job)
	for name, job := range stack.Jobs {
		if included[name] {
			projected.Jobs[name] = job
		}
	}
	projected.Services = make(map[string]manifest.Service)
	for name, service := range stack.Services {
		if included[name] {
			projected.Services[name] = service
		}
	}
	return &projected, nil
}

// contributeOperationIngress gives the routed services in an operation's
// compiled view the ingress wiring a full compile gives them: the edge labels,
// the edge network, and no published ports. jobOperationStack compiles without
// the ingress so the view leaves out the stack-wide edge service, and a chained
// restart that recreated a routed service from that view brought it back
// without its caddy labels, so the edge served an empty config. The wiring is
// computed from the whole stack because path routing numbers each service's
// label keys over every routed service; numbering only the view's would collide
// with the services left running.
func contributeOperationIngress(stack *manifest.Stack, compiled *CompiledStack) error {
	backend, err := edge.FromManifest(stack.Ingress)
	if err != nil {
		return err
	}
	_, hadEdge := compiled.Compose.Services["edge"]
	if err := backend.Contribute(stack, &compiled.Compose); err != nil {
		return err
	}
	if !hadEdge {
		delete(compiled.Compose.Services, "edge")
	}
	return nil
}

func stackNodeDependencies(stack *manifest.Stack, name string) []string {
	if job, ok := stack.Jobs[name]; ok {
		return job.DependsOn
	}
	if service, ok := stack.Services[name]; ok {
		return append(append([]string(nil), service.After...), service.DependsOn...)
	}
	return nil
}

func (p *Platform) updateJobRun(id string, fn func(*api.JobRunOperation)) {
	p.jobRunsMu.Lock()
	op := p.jobRuns[id]
	// Copy-on-write the Nodes slice so a mutation never writes through to a
	// backing array that a previously returned snapshot may still alias. The
	// stored record then never shares its Nodes array with any escaped clone.
	op.Nodes = append([]api.JobRunNode(nil), op.Nodes...)
	fn(&op)
	p.jobRuns[id] = op
	p.jobRunsMu.Unlock()
}

const (
	jobRunTimeoutEnv     = "ANGEE_JOB_TIMEOUT"
	defaultJobRunTimeout = 30 * time.Minute
)

// jobRunTimeout resolves the wall-clock bound applied to a detached job run.
// It mirrors the ANGEE_GIT_TIMEOUT convention: a package default overridable
// by an env var holding a Go duration. A value of 0 (or a malformed/negative
// value) disables the bound, matching the git timeout's escape hatch.
func jobRunTimeout() time.Duration {
	raw, ok := os.LookupEnv(jobRunTimeoutEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultJobRunTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < 0 {
		return defaultJobRunTimeout
	}
	return timeout
}

// jobRunContext derives the execution context for a detached job run. A
// positive timeout yields a context that expires after it; a zero timeout
// leaves the parent unbounded. The returned cancel func must always be called
// (defer) to release the timer and avoid a context leak.
func jobRunContext(parent context.Context) (context.Context, context.CancelFunc, time.Duration) {
	timeout := jobRunTimeout()
	if timeout <= 0 {
		return parent, func() {}, 0
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, timeout
}

func (p *Platform) executeJobRun(ctx context.Context, id string, release func()) {
	defer release()
	defer func() { p.jobRunsMu.Lock(); p.activeJobRun = false; p.jobRunsMu.Unlock() }()
	// executeJobRun runs under the daemon-lifetime detachedContext and holds the
	// single-slot mutation lease until it returns. A container job blocks on
	// `docker compose wait` with no timeout, so a job whose container never
	// exits would hold the lease (and activeJobRun) forever, rejecting every
	// other lifecycle mutation until the daemon restarts. Bound the execution.
	ctx, cancel, timeout := jobRunContext(ctx)
	defer cancel()
	p.jobRunsMu.RLock()
	stack := p.jobRunStacks[id]
	compiled := p.jobRunCompiled[id]
	p.jobRunsMu.RUnlock()
	if stack == nil || compiled == nil {
		p.finishJobRun(id, errors.New("job operation runtime snapshot is unavailable"))
		return
	}
	err := p.runJobGraph(ctx, id, stack, compiled)
	if timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		p.finishTimedOutJobRun(id, timeout)
		return
	}
	p.finishJobRun(id, err)
}

// runJobGraph drives the operation's node graph to completion, marking node
// states as it goes, and returns a non-nil error if the run failed. It never
// finalizes the operation itself: the caller owns finishJobRun so a timeout can
// override the outcome with a clear message.
func (p *Platform) runJobGraph(ctx context.Context, id string, stack *manifest.Stack, compiled *CompiledStack) (runErr error) {
	op, _ := p.JobRunGet(ctx, id)
	done := map[string]bool{}
	failed := map[string]bool{}
	// failures says why each node that failed, or was blocked by something
	// other than a failed node, did not run, for the run's error.
	var failures []string
	p.updateJobRun(id, func(o *api.JobRunOperation) { o.Status = api.JobRunRunning })
	held := map[string]bool{}
	for _, node := range op.Nodes {
		if node.Kind == "job" {
			for _, name := range stack.Jobs[node.Name].Holds {
				held[name] = true
			}
		}
	}
	// A recovery can fail after starting a held service. Restore the hold on
	// every attempted recovery, even when the operation context was canceled.
	resuming := map[string]bool{}
	defer func() {
		if runErr == nil {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtime.GracefulWaitDelay)
		defer cancel()
		for i, node := range op.Nodes {
			if !resuming[node.Name] {
				continue
			}
			if err := p.stopJobHeldService(cleanupContext, stack, compiled, node.Name); err != nil {
				p.setNode(id, i, api.JobRunFailed, "recovery failed; hold stop failed: "+err.Error())
				runErr = errors.Join(runErr, fmt.Errorf("restore hold on service %q: %w", node.Name, err))
			} else {
				p.setNode(id, i, api.JobRunBlocked, "held stopped; service recovery failed")
			}
		}
		latest, _ := p.JobRunGet(cleanupContext, id)
		for i, node := range latest.Nodes {
			if node.Status == api.JobRunPending || node.Status == api.JobRunRunning {
				message := "operation failed"
				if held[node.Name] {
					message = "held stopped; operation failed"
				}
				p.setNode(id, i, api.JobRunBlocked, message)
			}
		}
	}()
	// Stop each target separately so a partial failure has an accurate receipt.
	// A failed stop prevents every command and recovery, while other successful
	// stops stay in force. The existing mutation lease protects this interval.
	var stopErrors []error
	for i, node := range op.Nodes {
		if !held[node.Name] {
			continue
		}
		if err := p.stopJobHeldService(ctx, stack, compiled, node.Name); err != nil {
			failed[node.Name] = true
			p.setNode(id, i, api.JobRunFailed, "hold stop failed: "+err.Error())
			stopErrors = append(stopErrors, fmt.Errorf("stop held service %q: %w", node.Name, err))
		} else {
			p.setNode(id, i, api.JobRunPending, "held stopped")
			if !op.ChainedRestart {
				done[node.Name] = true
				p.setNode(id, i, api.JobRunSucceeded, "held stopped; no chained restart requested")
			}
		}
	}
	block := func(i int, node api.JobRunNode, reason string) {
		if held[node.Name] {
			reason = "held stopped; " + reason
		}
		p.setNode(id, i, api.JobRunBlocked, reason)
	}
	if err := errors.Join(stopErrors...); err != nil {
		for i, node := range op.Nodes {
			if !done[node.Name] && !failed[node.Name] {
				block(i, node, "hold stop failed")
			}
		}
		return err
	}
	remaining := len(op.Nodes) - len(done)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		progressed := false
		for i, n := range op.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if done[n.Name] || failed[n.Name] {
				continue
			}
			if len(held) > 0 && n.Kind == "service" {
				jobsPending, jobsFailed := false, false
				for _, job := range op.Nodes {
					if job.Kind == "job" {
						jobsPending = jobsPending || (!done[job.Name] && !failed[job.Name])
						jobsFailed = jobsFailed || failed[job.Name]
					}
				}
				if jobsFailed {
					block(i, n, "job chain failed")
					failed[n.Name] = true
					remaining--
					progressed = true
					continue
				}
				if jobsPending {
					continue
				}
			}
			deps := stackNodeDependencies(stack, n.Name)
			blocked := false
			blockedMessage := "dependency failed"
			ready := true
			for _, d := range deps {
				if failed[d] {
					blocked = true
				}
				if containsOperationNode(op.Nodes, d) && !done[d] {
					ready = false
				} else if !containsOperationNode(op.Nodes, d) {
					satisfied, dependencyErr := p.dependencySatisfied(ctx, stack, compiled, d)
					if dependencyErr != nil || !satisfied {
						blocked = true
						blockedMessage = "declared prerequisite is not ready in the current runtime"
					}
				}
			}
			if blocked {
				failed[n.Name] = true
				remaining--
				progressed = true
				block(i, n, blockedMessage)
				if blockedMessage != "dependency failed" {
					failures = append(failures, fmt.Sprintf("%s %q was blocked: %s", n.Kind, n.Name, blockedMessage))
				}
				continue
			}
			if !ready {
				continue
			}
			p.setNode(id, i, api.JobRunRunning, "")
			p.updateJobRun(id, func(o *api.JobRunOperation) { o.CurrentStep = n.Name })
			var out []byte
			var err error
			if n.Kind == "job" {
				out, err = p.runJob(ctx, stack, compiled, n.Name)
			} else {
				if held[n.Name] {
					resuming[n.Name] = true
				}
				err = p.applyService(ctx, stack, compiled, n.Name)
			}
			if err != nil {
				failed[n.Name] = true
				p.setNode(id, i, api.JobRunFailed, err.Error())
				if len(held) > 0 && n.Kind == "service" {
					return fmt.Errorf("recover service %q: %w", n.Name, err)
				}
				if n.Name != op.RootJob {
					failures = append(failures, fmt.Sprintf("%s %q failed: %s", n.Kind, n.Name, firstLine(err.Error())))
				}
			} else {
				done[n.Name] = true
				message := ""
				if held[n.Name] {
					message = "resumed after job chain"
				}
				p.setNode(id, i, api.JobRunSucceeded, message)
			}
			if n.Name == op.RootJob {
				p.updateJobRun(id, func(o *api.JobRunOperation) { o.Output = boundedJobOutput(out) })
				if err != nil {
					for pendingIndex, pending := range op.Nodes {
						if pending.Name != n.Name && !done[pending.Name] && !failed[pending.Name] {
							block(pendingIndex, pending, "root job failed")
							failed[pending.Name] = true
						}
					}
					return &OperationError{Code: CodeJobFailed, Job: op.RootJob, Summary: err.Error(), Err: err}
				}
			}
			remaining--
			progressed = true
		}
		if !progressed {
			return &OperationError{Code: CodeJobFailed, Job: op.RootJob, Summary: "job dependency graph cannot make progress"}
		}
	}
	if len(failed) > 0 {
		if len(failures) == 0 {
			failures = []string{"one or more dependents failed"}
		}
		return &OperationError{
			Code:    CodeJobFailed,
			Cause:   CauseDependencyFailed,
			Job:     op.RootJob,
			Hint:    "Check the failed node's logs with `angee logs <name>`, then run the job again.",
			Summary: strings.Join(failures, "; "),
		}
	}
	return nil
}

// stopJobHeldService dispatches through the same runtime owners as service
// actions, using the operation's stack snapshot rather than reloading inputs.
func (p *Platform) stopJobHeldService(ctx context.Context, stack *manifest.Stack, compiled *CompiledStack, name string) error {
	service := stack.Services[name]
	backend := p.composeBackend
	if service.Runtime == manifest.RuntimeLocal {
		backend = p.procBackend
	}
	configuration, err := p.compiledRuntimeConfiguration(compiled, service.Runtime)
	if err != nil {
		return err
	}
	return backend.Stop(ctx, runtime.Target{Root: p.root, Services: []string{name}, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack), Configuration: configuration})
}

func (p *Platform) dependencySatisfied(ctx context.Context, stack *manifest.Stack, compiled *CompiledStack, name string) (bool, error) {
	if service, ok := stack.Services[name]; ok {
		backend, request, err := p.operationRuntime(stack, compiled, service.Runtime)
		if err != nil {
			return false, err
		}
		statuses, err := backend.Status(ctx, request)
		if err != nil {
			return false, err
		}
		for _, state := range statuses {
			if state.Name == name {
				return strings.EqualFold(state.State, "running") && (service.Ready == nil || state.Health == "healthy"), nil
			}
		}
		return false, nil
	}
	job, ok := stack.Jobs[name]
	if !ok {
		return false, nil
	}
	backend := p.composeBackend
	request := runtime.StatusRequest{Root: p.root, EnvFile: p.runtimeEnvFile(stack)}
	if job.Runtime == manifest.RuntimeLocal {
		backend = p.procBackend
		request.ControlPort = processComposeControlPort(stack)
	}
	configuration, err := p.compiledRuntimeConfiguration(compiled, job.Runtime)
	if err != nil {
		return false, err
	}
	request.Configuration = configuration
	statuses, err := backend.Status(ctx, request)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		state := strings.ToLower(strings.TrimSpace(status.State))
		terminal := state == "exited" || state == "completed" || state == "completed successfully"
		if status.Name == name && terminal && status.ExitCode != nil && *status.ExitCode == 0 {
			return true, nil
		}
	}
	return false, nil
}

const maxPersistedJobOutput = 1 << 20
const maxPersistedJobError = 4 << 10

func boundedJobOutput(output []byte) string {
	output = []byte(logctx.RedactText(string(output)))
	if len(output) <= maxPersistedJobOutput {
		return string(output)
	}
	return string(output[len(output)-maxPersistedJobOutput:])
}
func boundedJobError(message string) string {
	message = logctx.RedactText(message)
	if len(message) <= maxPersistedJobError {
		return message
	}
	return message[:maxPersistedJobError]
}
func containsOperationNode(ns []api.JobRunNode, name string) bool {
	for _, n := range ns {
		if n.Name == name {
			return true
		}
	}
	return false
}
func (p *Platform) setNode(id string, i int, status api.JobRunStatus, msg string) {
	p.updateJobRun(id, func(o *api.JobRunOperation) { o.Nodes[i].Status = status; o.Nodes[i].Message = boundedJobError(msg) })
}
func (p *Platform) finishJobRun(id string, err error) {
	now := time.Now().UTC()
	p.updateJobRun(id, func(o *api.JobRunOperation) {
		o.EndedAt = &now
		o.CurrentStep = ""
		if err != nil {
			o.Status = api.JobRunFailed
			o.Error = boundedJobError(err.Error())
			o.ErrorCode = CodeJobFailed
			var opErr *OperationError
			if errors.As(err, &opErr) {
				o.ErrorCode = opErr.Code
				o.ErrorCause = opErr.Cause
			}
		} else {
			o.Status = api.JobRunSucceeded
		}
	})
	p.jobRunsMu.Lock()
	delete(p.jobRunStacks, id)
	delete(p.jobRunCompiled, id)
	p.jobRunsMu.Unlock()
}

// finishTimedOutJobRun finalizes a run whose execution context hit its
// deadline. It records a clear timeout error and marks every node that had not
// already reached a terminal state (running -> failed, pending -> blocked), so
// no node dangles as pending or running. The lease is released by the caller's
// deferred release in executeJobRun.
func (p *Platform) finishTimedOutJobRun(id string, timeout time.Duration) {
	now := time.Now().UTC()
	message := fmt.Sprintf("job run timed out after %s (raise %s, or set it to 0, to allow longer runs)", timeout, jobRunTimeoutEnv)
	p.updateJobRun(id, func(o *api.JobRunOperation) {
		o.EndedAt = &now
		o.CurrentStep = ""
		o.Status = api.JobRunFailed
		o.Error = boundedJobError(message)
		o.ErrorCode = CodeTimeout
		for i := range o.Nodes {
			switch o.Nodes[i].Status {
			case api.JobRunRunning:
				o.Nodes[i].Status = api.JobRunFailed
				o.Nodes[i].Message = boundedJobError(message)
			case api.JobRunPending:
				o.Nodes[i].Status = api.JobRunBlocked
				o.Nodes[i].Message = boundedJobError("job run timed out before this node started")
			}
		}
	})
	p.jobRunsMu.Lock()
	delete(p.jobRunStacks, id)
	delete(p.jobRunCompiled, id)
	p.jobRunsMu.Unlock()
}
func (p *Platform) applyService(ctx context.Context, stack *manifest.Stack, compiled *CompiledStack, name string) error {
	s := stack.Services[name]
	target := runtime.Target{Root: p.root, Services: []string{name}, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack)}
	backend := p.composeBackend
	if s.Runtime == manifest.RuntimeLocal {
		backend = p.procBackend
	}
	if err := p.applyRuntimeConfiguration(ctx, compiled, s.Runtime, backend, target); err != nil {
		return fmt.Errorf("apply service %s: %w", name, err)
	}
	return p.waitServiceReady(ctx, stack, compiled, name)
}
func (p *Platform) waitServiceReady(ctx context.Context, stack *manifest.Stack, compiled *CompiledStack, name string) error {
	wait := 30 * time.Second
	if probe := stack.Services[name].Ready; probe != nil {
		normalized := probe.Normalized()
		startPeriod, _ := time.ParseDuration(normalized.StartPeriod)
		interval, _ := time.ParseDuration(normalized.Interval)
		timeout, _ := time.ParseDuration(normalized.Timeout)
		wait = startPeriod + time.Duration(*normalized.Retries)*interval + timeout
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("service %s did not become ready", name)
		case <-tick.C:
			backend, request, err := p.operationRuntime(stack, compiled, stack.Services[name].Runtime)
			if err != nil {
				continue
			}
			statuses, err := backend.Status(ctx, request)
			if err == nil {
				for _, s := range statuses {
					if s.Name == name && strings.EqualFold(s.State, "running") && (stack.Services[name].Ready == nil || s.Health == "healthy") {
						return nil
					}
				}
			}
		}
	}
}

func (p *Platform) compiledRuntimeConfiguration(compiled *CompiledStack, target manifest.Runtime) ([]byte, error) {
	if target == manifest.RuntimeLocal {
		return proccompose.Marshal(compiled.ProcessCompose)
	}
	return compose.Marshal(compiled.Compose)
}

func (p *Platform) operationRuntime(stack *manifest.Stack, compiled *CompiledStack, target manifest.Runtime) (runtime.Backend, runtime.StatusRequest, error) {
	configuration, err := p.compiledRuntimeConfiguration(compiled, target)
	request := runtime.StatusRequest{Root: p.root, EnvFile: p.runtimeEnvFile(stack), Configuration: configuration}
	backend := p.composeBackend
	if target == manifest.RuntimeLocal {
		backend = p.procBackend
		request.ControlPort = processComposeControlPort(stack)
	}
	return backend, request, err
}

func (p *Platform) runJob(ctx context.Context, stack *manifest.Stack, compiled *CompiledStack, name string) ([]byte, error) {
	job, ok := stack.Jobs[name]
	if !ok {
		return nil, &NotFoundError{Kind: "job", Name: name}
	}
	spec := runtime.JobSpec{Name: name, Transient: job.Capture != nil}
	if job.Runtime == manifest.RuntimeLocal {
		configuration, err := proccompose.Marshal(compiled.ProcessCompose)
		if err != nil {
			return nil, err
		}
		spec.Configuration = configuration
		target := runtime.Target{Root: p.root, Services: []string{name}, EnvFile: p.runtimeEnvFile(stack), ControlPort: processComposeControlPort(stack)}
		p.jobOutput.status(name, "running")
		finish := logctx.Step(ctx, "running job "+name)
		runner, ok := p.procBackend.(runtime.JobBackend)
		if !ok {
			return nil, fmt.Errorf("local job execution is unavailable from the configured process-compose backend")
		}
		out, err := runner.RunJob(ctx, target, spec)
		finish(err)
		if err != nil {
			p.jobOutput.status(name, "failed")
		} else {
			p.jobOutput.status(name, "finished")
		}
		return out, err
	}
	if job.Runtime == manifest.RuntimeContainer {
		configuration, err := compose.Marshal(compiled.Compose)
		if err != nil {
			return nil, err
		}
		spec.Configuration = configuration
		p.jobOutput.status(name, "running")
		finish := logctx.Step(ctx, "running job "+name)
		runner, ok := p.composeBackend.(runtime.JobBackend)
		if !ok {
			return nil, fmt.Errorf("container job execution is unavailable from the configured compose backend")
		}
		out, err := runner.RunJob(ctx, runtime.Target{Root: p.root, EnvFile: p.runtimeEnvFile(stack)}, spec)
		finish(err)
		if err != nil {
			p.jobOutput.status(name, "failed")
			return out, fmt.Errorf("job container command failed: %w", err)
		}
		p.jobOutput.status(name, "finished")
		return out, nil
	}
	return nil, fmt.Errorf("job %q has unsupported runtime %q", name, job.Runtime)
}

func runLocalCommand(ctx context.Context, workdir string, command []string, env map[string]string, sink io.Writer) ([]byte, error) {
	if len(command) == 0 {
		return nil, &InvalidInputError{Field: "command", Reason: "command is empty"}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	addedEnv := make([]string, 0, len(env))
	for key, value := range env {
		entry := key + "=" + value
		cmd.Env = append(cmd.Env, entry)
		addedEnv = append(addedEnv, entry)
	}
	trace := logctx.TraceExec(ctx, command[0], command[1:], workdir, slog.Any("env", logctx.EnvKeys(addedEnv)))
	out, err := runCommand(cmd, sink)
	trace(out, err)
	if err != nil {
		return out, fmt.Errorf("job command failed: %w: %s", err, out)
	}
	return out, nil
}

func runCommand(cmd *exec.Cmd, sink io.Writer) ([]byte, error) {
	var captured bytes.Buffer
	output := io.Writer(&captured)
	if sink != nil {
		output = io.MultiWriter(&captured, sink)
	}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	out := captured.Bytes()
	return out, err
}

// jobOperation names a job run, such as `Run job "deps" with chained restart`.
func jobOperation(name string, chainedRestart bool) operation {
	title := fmt.Sprintf("Run job %q", name)
	if chainedRestart {
		title += " with chained restart"
	}
	return operation{name: "job.run", title: title, job: name}
}

// maxRetainedJobRuns bounds the receipts held in memory. A persisted backup
// receipt stays readable from state/job-runs after it leaves memory.
const maxRetainedJobRuns = 64

// rememberJobRun retains a job run's receipt and runtime snapshot and makes it
// the latest run. Caller holds jobRunsMu.
func (p *Platform) rememberJobRun(op api.JobRunOperation, stack *manifest.Stack, compiled *CompiledStack) {
	p.latestJobRun = op.ID
	p.retainJobRun(op)
	p.jobRunStacks[op.ID] = stack
	p.jobRunCompiled[op.ID] = compiled
}

// rememberBackupRun retains a backup's receipt, readable by its ID, without
// making it the latest run: that is the job runner's and the restart flow's.
// Caller holds jobRunsMu.
func (p *Platform) rememberBackupRun(op api.JobRunOperation) {
	p.retainJobRun(op)
}

// retainJobRun applies the shared receipt retention policy: the oldest receipt
// is dropped first, except the latest job run, which newer backups never evict.
// Caller holds jobRunsMu.
func (p *Platform) retainJobRun(op api.JobRunOperation) {
	p.jobRuns[op.ID] = op
	p.jobRunOrder = append(p.jobRunOrder, op.ID)
	if len(p.jobRunOrder) <= maxRetainedJobRuns {
		return
	}
	oldest := 0
	if p.jobRunOrder[oldest] == p.latestJobRun {
		oldest++
	}
	expired := p.jobRunOrder[oldest]
	p.jobRunOrder = slices.Delete(p.jobRunOrder, oldest, oldest+1)
	delete(p.jobRuns, expired)
	delete(p.jobRunStacks, expired)
	delete(p.jobRunCompiled, expired)
}
