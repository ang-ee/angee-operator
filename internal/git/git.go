package git

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type Client struct {
	Bin string
}

func New() Client {
	return Client{Bin: "git"}
}

func (c Client) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return out, nil
}

func (c Client) Clone(ctx context.Context, repo, dest string, args ...string) error {
	cmdArgs := append([]string{"clone"}, args...)
	cmdArgs = append(cmdArgs, repo, dest)
	_, err := c.Run(ctx, "", cmdArgs...)
	return err
}

func (c Client) CloneRef(ctx context.Context, repo, dest, ref string) error {
	args := []string{}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	return c.Clone(ctx, repo, dest, args...)
}

func (c Client) Fetch(ctx context.Context, dir string) error {
	_, err := c.Run(ctx, dir, "fetch", "--all", "--prune")
	return err
}

func (c Client) WorktreeAdd(ctx context.Context, repoDir, dest, ref string) error {
	args := []string{"worktree", "add", dest}
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

func (c Client) WorktreeAddBranch(ctx context.Context, repoDir, dest, branch, ref string) error {
	args := []string{"worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, dest)
	if ref != "" {
		args = append(args, ref)
	}
	_, err := c.Run(ctx, repoDir, args...)
	return err
}

func (c Client) CurrentRef(ctx context.Context, dir string) (string, error) {
	branch, ok, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	if ok {
		return branch, nil
	}
	out, err := c.Run(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (c Client) CurrentBranch(ctx context.Context, dir string) (string, bool, error) {
	out, err := c.Run(ctx, dir, "branch", "--show-current")
	if err != nil {
		return "", false, err
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", false, nil
	}
	return branch, true, nil
}

func (c Client) Upstream(ctx context.Context, dir string) (string, bool, error) {
	out, err := c.Run(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return "", false, nil
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return "", false, nil
	}
	return ref, true, nil
}

func (c Client) AheadCount(ctx context.Context, dir, base string) (int, error) {
	ahead, _, err := c.AheadBehind(ctx, dir, base)
	return ahead, err
}

func (c Client) AheadBehind(ctx context.Context, dir, base string) (ahead int, behind int, err error) {
	out, err := c.Run(ctx, dir, "rev-list", "--left-right", "--count", base+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("parse git ahead/behind count %q: expected two fields", strings.TrimSpace(string(out)))
	}
	behind, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse git behind count %q: %w", fields[0], err)
	}
	ahead, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse git ahead count %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}

func (c Client) Config(ctx context.Context, dir, key string) (string, bool, error) {
	out, err := c.Run(ctx, dir, "config", "--get", key)
	if err != nil {
		return "", false, nil
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

func (c Client) Remotes(ctx context.Context, dir string) ([]string, error) {
	out, err := c.Run(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	remotes := []string{}
	for _, line := range lines {
		remote := strings.TrimSpace(line)
		if remote != "" {
			remotes = append(remotes, remote)
		}
	}
	return remotes, nil
}

func (c Client) PushRemote(ctx context.Context, dir string) (string, error) {
	branch, hasBranch, err := c.CurrentBranch(ctx, dir)
	if err != nil {
		return "", err
	}
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".pushRemote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	if remote, ok, err := c.Config(ctx, dir, "remote.pushDefault"); err != nil {
		return "", err
	} else if ok {
		return remote, nil
	}
	if hasBranch {
		if remote, ok, err := c.Config(ctx, dir, "branch."+branch+".remote"); err != nil {
			return "", err
		} else if ok {
			return remote, nil
		}
	}
	remotes, err := c.Remotes(ctx, dir)
	if err != nil {
		return "", err
	}
	for _, remote := range remotes {
		if remote == "origin" {
			return remote, nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if len(remotes) == 0 {
		return "", fmt.Errorf("no git remotes configured")
	}
	if hasBranch {
		return "", fmt.Errorf("multiple git remotes configured; set branch.%s.pushRemote or remote.pushDefault", branch)
	}
	return "", fmt.Errorf("multiple git remotes configured; set remote.pushDefault")
}

func (c Client) Dirty(ctx context.Context, dir string) (bool, error) {
	out, err := c.Run(ctx, dir, "status", "--short")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (c Client) Pull(ctx context.Context, dir string) error {
	_, err := c.Run(ctx, dir, "pull", "--ff-only")
	return err
}

func (c Client) Push(ctx context.Context, dir string, ref string) error {
	args := []string{"push"}
	if ref != "" {
		remote, err := c.PushRemote(ctx, dir)
		if err != nil {
			return err
		}
		args = append(args, remote, ref)
	}
	_, err := c.Run(ctx, dir, args...)
	return err
}

func (c Client) PushSetUpstream(ctx context.Context, dir string, ref string) error {
	if ref == "" {
		return c.Push(ctx, dir, ref)
	}
	remote, err := c.PushRemote(ctx, dir)
	if err != nil {
		return err
	}
	_, err = c.Run(ctx, dir, "push", "-u", remote, ref)
	return err
}
