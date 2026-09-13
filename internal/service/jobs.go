package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

func (p *Platform) JobRunStart(ctx context.Context, name string, inputs map[string]string, chainedRestart bool) (api.JobRunOperation, error) {
	ctx, release, err := p.beginMutation(ctx, "job")
	if err != nil {
		return api.JobRunOperation{}, err
	}
	jobInputs := map[string]map[string]string(nil)
	if len(inputs) != 0 {
		jobInputs = map[string]map[string]string{name: inputs}
	}
	compiled, err := p.stackPrepare(ctx, jobInputs, false, name, chainedRestart)
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
	p.jobRunsMu.Lock()
	if p.activeJobRun {
		p.jobRunsMu.Unlock()
		release()
		return api.JobRunOperation{}, &InvalidInputError{Field: "job", Reason: "another job operation is active"}
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
	p.jobRuns[id] = op
	p.jobRunStacks[id] = stack
	p.jobRunCompiled[id] = compiled
	p.jobRunOrder = append(p.jobRunOrder, id)
	if len(p.jobRunOrder) > 64 {
		expired := p.jobRunOrder[0]
		p.jobRunOrder = p.jobRunOrder[1:]
		delete(p.jobRuns, expired)
		delete(p.jobRunStacks, expired)
		delete(p.jobRunCompiled, expired)
	}
	p.latestJobRun = id
	p.jobRunsMu.Unlock()
	operationContext := ctx
	if p.detachedContext != nil {
		operationContext = p.detachedContext
	}
	go p.executeJobRun(operationContext, id, release)
	return cloneJobRun(op), nil
}

// JobRun preserves the synchronous local API while the operator transports use
// JobRunStart to obtain a durable receipt immediately.
func (p *Platform) JobRun(ctx context.Context, name string, inputs map[string]string) ([]byte, error) {
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
		return []byte(op.Output), errors.New(op.Error)
	}
	return []byte(op.Output), nil
}

func (p *Platform) JobRunGet(_ context.Context, id string) (api.JobRunOperation, error) {
	p.jobRunsMu.RLock()
	defer p.jobRunsMu.RUnlock()
	op, ok := p.jobRuns[id]
	if !ok {
		return api.JobRunOperation{}, &NotFoundError{Kind: "job run", Name: id}
	}
	return cloneJobRun(op), nil
}

func (p *Platform) LatestJobRun(_ context.Context) (*api.JobRunOperation, error) {
	p.jobRunsMu.RLock()
	defer p.jobRunsMu.RUnlock()
	if p.latestJobRun == "" {
		return nil, nil
	}
	op := cloneJobRun(p.jobRuns[p.latestJobRun])
	return &op, nil
}

func cloneJobRun(op api.JobRunOperation) api.JobRunOperation {
	op.Nodes = append([]api.JobRunNode(nil), op.Nodes...)
	return op
}

func (p *Platform) JobRunPreview(_ context.Context, name string, chained bool) (api.JobRunPreview, error) {
	stack, err := p.LoadStack()
	if err != nil {
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
		for _, dependency := range stackNodeDependencies(stack, name) {
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
	fn(&op)
	p.jobRuns[id] = op
	p.jobRunsMu.Unlock()
}

func (p *Platform) executeJobRun(ctx context.Context, id string, release func()) {
	defer release()
	defer func() { p.jobRunsMu.Lock(); p.activeJobRun = false; p.jobRunsMu.Unlock() }()
	p.jobRunsMu.RLock()
	stack := p.jobRunStacks[id]
	compiled := p.jobRunCompiled[id]
	p.jobRunsMu.RUnlock()
	if stack == nil || compiled == nil {
		p.finishJobRun(id, errors.New("job operation runtime snapshot is unavailable"))
		return
	}
	op, _ := p.JobRunGet(ctx, id)
	done := map[string]bool{}
	failed := map[string]bool{}
	p.updateJobRun(id, func(o *api.JobRunOperation) { o.Status = api.JobRunRunning })
	remaining := len(op.Nodes)
	for remaining > 0 {
		progressed := false
		for i, n := range op.Nodes {
			if done[n.Name] || failed[n.Name] {
				continue
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
				p.setNode(id, i, api.JobRunBlocked, blockedMessage)
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
				err = p.applyService(ctx, stack, compiled, n.Name)
			}
			if err != nil {
				failed[n.Name] = true
				p.setNode(id, i, api.JobRunFailed, err.Error())
			} else {
				done[n.Name] = true
				p.setNode(id, i, api.JobRunSucceeded, "")
			}
			if n.Name == op.RootJob {
				p.updateJobRun(id, func(o *api.JobRunOperation) { o.Output = boundedJobOutput(out) })
				if err != nil {
					for pendingIndex, pending := range op.Nodes {
						if pending.Name != n.Name && !done[pending.Name] && !failed[pending.Name] {
							failed[pending.Name] = true
							p.setNode(id, pendingIndex, api.JobRunBlocked, "root job failed")
						}
					}
					p.finishJobRun(id, err)
					return
				}
			}
			remaining--
			progressed = true
		}
		if !progressed {
			p.finishJobRun(id, fmt.Errorf("job dependency graph cannot make progress"))
			return
		}
	}
	if len(failed) > 0 {
		p.finishJobRun(id, fmt.Errorf("one or more dependents failed"))
		return
	}
	p.finishJobRun(id, nil)
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
	if len(output) <= maxPersistedJobOutput {
		return string(output)
	}
	return string(output[len(output)-maxPersistedJobOutput:])
}
func boundedJobError(message string) string {
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
		} else {
			o.Status = api.JobRunSucceeded
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
	configuration, err := p.compiledRuntimeConfiguration(compiled, s.Runtime)
	if err != nil {
		return err
	}
	target.Configuration = configuration
	applier, ok := backend.(runtime.ApplyBackend)
	if !ok {
		return fmt.Errorf("service %s cannot be applied by the configured %s backend", name, s.Runtime)
	}
	if err := applier.Apply(ctx, target); err != nil {
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
	if job.Runtime == manifest.RuntimeLocal {
		configuration, err := proccompose.Marshal(compiled.ProcessCompose)
		if err != nil {
			return nil, err
		}
		spec := runtime.JobSpec{Name: name, Configuration: configuration}
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
		spec := runtime.JobSpec{Name: name, Configuration: configuration}
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
