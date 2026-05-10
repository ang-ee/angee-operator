package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/manifest"
	mountx "github.com/fyltr/angee/internal/mount"
	"github.com/fyltr/angee/internal/secrets"
	"github.com/fyltr/angee/internal/substitute"
)

func (p *Platform) JobList(ctx context.Context) ([]api.JobState, error) {
	status, err := p.StackStatus(ctx)
	if err != nil {
		return nil, err
	}
	jobs := make([]api.JobState, 0, len(status.Jobs))
	for _, name := range sortedKeys(status.Jobs) {
		jobs = append(jobs, status.Jobs[name])
	}
	return jobs, nil
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
		return runLocalCommand(ctx, workdir, command, env)
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
		return cmd.CombinedOutput()
	}
	return nil, fmt.Errorf("job %q has unsupported runtime %q", name, job.Runtime)
}

func runLocalCommand(ctx context.Context, workdir string, command []string, env map[string]string) ([]byte, error) {
	if len(command) == 0 {
		return nil, &InvalidInputError{Field: "command", Reason: "command is empty"}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = workdir
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("job command failed: %w: %s", err, out)
	}
	return out, nil
}
