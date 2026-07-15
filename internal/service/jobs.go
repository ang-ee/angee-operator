package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	mountx "github.com/ang-ee/angee-operator/internal/mount"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/secrets"
	"github.com/ang-ee/angee-operator/internal/substitute"
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

func (p *Platform) JobRun(ctx context.Context, name string, inputs map[string]string) ([]byte, error) {
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	job, ok := stack.Jobs[name]
	if !ok {
		return nil, &NotFoundError{Kind: "job", Name: name}
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return nil, err
	}
	resolvedSecrets, err := secrets.ResolveDeclarations(ctx, backend, stack.Secrets, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	subCtx := baseSubstitutionContext(stack, p.root, resolvedSecrets, nil)
	subCtx.Inputs = inputs
	subCtx.Name = name
	command, err := substitute.ResolveSlice(job.Command, subCtx)
	if err != nil {
		return nil, err
	}
	env, err := substitute.ResolveMap(job.Env, subCtx)
	if err != nil {
		return nil, err
	}
	workdir, err := substitute.Resolve(job.Workdir, subCtx)
	if err != nil {
		return nil, err
	}
	mounts, err := substitute.ResolveSlice([]string(job.Mounts), subCtx)
	if err != nil {
		return nil, err
	}
	resolver := resourceResolver(stack, p.root)
	if job.Runtime == manifest.RuntimeLocal {
		localEnv, err := localMountEnv(mounts, resolver)
		if err != nil {
			return nil, err
		}
		if env == nil {
			env = map[string]string{}
		}
		for key, value := range localEnv {
			env[key] = value
		}
		workdir, err = mountx.ResolveWorkdir(workdir, resolver)
		if err != nil {
			return nil, err
		}
		if workdir != "" && !filepath.IsAbs(workdir) {
			workdir = filepath.Join(p.root, workdir)
		}
		p.jobOutput.status(name, "running")
		out, err := runLocalCommand(ctx, workdir, command, env, p.jobOutput)
		if err != nil {
			p.jobOutput.status(name, "failed")
		} else {
			p.jobOutput.status(name, "finished")
		}
		return out, err
	}
	if job.Runtime == manifest.RuntimeContainer {
		args := []string{"run", "--rm"}
		for key, value := range env {
			args = append(args, "-e", key+"="+value)
		}
		args = append(args, job.Image)
		args = append(args, command...)
		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Dir = p.root
		p.jobOutput.status(name, "running")
		out, err := runCommand(cmd, p.jobOutput)
		if err != nil {
			p.jobOutput.status(name, "failed")
			return out, fmt.Errorf("job container command failed: %w: %s", err, out)
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
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := runCommand(cmd, sink)
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
