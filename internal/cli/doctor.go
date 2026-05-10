package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyltr/angee/internal/copierx"
	"github.com/fyltr/angee/internal/manifest"
	"github.com/fyltr/angee/internal/stackroot"
	"github.com/spf13/cobra"
)

type doctorStatus string

const (
	doctorOK    doctorStatus = "ok"
	doctorWarn  doctorStatus = "warn"
	doctorError doctorStatus = "error"
)

type doctorReport struct {
	Version string        `json:"version"`
	Root    string        `json:"root"`
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
}

type doctorCheck struct {
	Name   string       `json:"name"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail"`
	Hint   string       `json:"hint,omitempty"`
}

type doctorSummary struct {
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Errors   int `json:"errors"`
}

type doctorRunner struct {
	checks []doctorCheck
}

func doctorCommand(stdout io.Writer, root *string, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local angee development prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report := runDoctor(cmd.Context(), *root)
			if *jsonOutput {
				if err := writeJSON(stdout, report); err != nil {
					return err
				}
			} else {
				if err := writeDoctorReport(stdout, report); err != nil {
					return err
				}
			}
			if report.Summary.Errors > 0 {
				return fmt.Errorf("doctor found %d error(s)", report.Summary.Errors)
			}
			return nil
		},
	}
}

func runDoctor(ctx context.Context, requestedRoot string) doctorReport {
	runner := &doctorRunner{}
	root := requestedRoot
	if root == "" {
		root = "."
	}
	resolvedRoot, err := stackroot.Resolve(root)
	if err != nil {
		runner.add("root", doctorError, err.Error(), "Pass --root with the ANGEE_ROOT containing angee.yaml.")
		resolvedRoot = root
	} else {
		runner.add("root", doctorOK, displayPath(resolvedRoot), "")
	}
	absRoot, err := filepath.Abs(resolvedRoot)
	if err != nil {
		runner.add("root.absolute", doctorError, err.Error(), "")
		absRoot = resolvedRoot
	}

	runner.checkTools(ctx)
	stack := runner.checkManifest(absRoot)
	if stack != nil {
		runner.checkLocalSources(absRoot, stack)
		runner.checkPorts(stack)
		runner.checkPortPools(stack)
	}
	runner.checkGitIgnores(ctx)
	runner.checkTemplates()

	report := doctorReport{
		Version: Version,
		Root:    absRoot,
		Checks:  runner.checks,
	}
	report.Summary = summarizeDoctorChecks(report.Checks)
	return report
}

func (r *doctorRunner) add(name string, status doctorStatus, detail string, hint string) {
	r.checks = append(r.checks, doctorCheck{
		Name:   name,
		Status: status,
		Detail: detail,
		Hint:   hint,
	})
}

func (r *doctorRunner) checkTools(ctx context.Context) {
	tools := []struct {
		name string
		args []string
		hint string
	}{
		{name: "git", args: []string{"--version"}, hint: "Required for source and workspace commands."},
		{name: "go", args: []string{"version"}, hint: "Required to build angee-go and to auto-install process-compose."},
		{name: "uv", args: []string{"--version"}, hint: "Required by the bundled Django dev stack."},
		{name: "node", args: []string{"--version"}, hint: "Required by the bundled React/Vite dev stack."},
		{name: "pnpm", args: []string{"--version"}, hint: "Required by the bundled React/Vite dev stack."},
		{name: "npx", args: []string{"--version"}, hint: "Required by the bundled playwright-mcp service."},
		{name: "docker", args: []string{"--version"}, hint: "Required for container runtime services."},
		{name: "process-compose", args: []string{"--version"}, hint: "Required for local dev runtime services; angee can prompt to install it when needed."},
	}
	for _, tool := range tools {
		path, err := exec.LookPath(tool.name)
		if err != nil {
			r.add("tool."+tool.name, doctorWarn, "not found on PATH", tool.hint)
			continue
		}
		version, err := commandVersion(ctx, path, tool.args)
		if err != nil {
			r.add("tool."+tool.name, doctorWarn, fmt.Sprintf("%s found but version check failed: %v", path, err), tool.hint)
			continue
		}
		if version == "" {
			version = path
		}
		r.add("tool."+tool.name, doctorOK, version, "")
	}
}

func commandVersion(ctx context.Context, path string, args []string) (string, error) {
	childCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, path, args...)
	out, err := cmd.CombinedOutput()
	if childCtx.Err() != nil {
		return "", childCtx.Err()
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

func (r *doctorRunner) checkManifest(root string) *manifest.Stack {
	path := manifest.Path(root)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			r.add("manifest", doctorWarn, fmt.Sprintf("%s does not exist", displayPath(path)), "Run `angee init --dev --yes` or `angee stack init <template>` before starting services.")
			return nil
		}
		r.add("manifest", doctorError, err.Error(), "")
		return nil
	}
	stack, err := manifest.LoadFile(path)
	if err != nil {
		r.add("manifest", doctorError, err.Error(), "Fix angee.yaml before running stack or workspace commands.")
		return nil
	}
	detail := fmt.Sprintf("%s (%d services, %d jobs, %d workspaces)", stack.Name, len(stack.Services), len(stack.Jobs), len(stack.Workspaces))
	r.add("manifest", doctorOK, detail, "")
	return stack
}

func (r *doctorRunner) checkLocalSources(root string, stack *manifest.Stack) {
	names := make([]string, 0, len(stack.Sources))
	for name := range stack.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		source := stack.Sources[name]
		if source.Kind != "local" {
			continue
		}
		path := manifest.ResolvePath(root, source.Path)
		if _, err := os.Stat(path); err != nil {
			status := doctorWarn
			hint := "Check the source path in angee.yaml or re-render the stack template."
			if name == "app" {
				hint = "The app source must exist before local dev services can start."
			}
			r.add("source."+name, status, fmt.Sprintf("%s is missing", displayPath(path)), hint)
			continue
		}
		r.add("source."+name, doctorOK, displayPath(path), "")
	}
}

func (r *doctorRunner) checkPorts(stack *manifest.Stack) {
	names := make([]string, 0, len(stack.Ports))
	for name := range stack.Ports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		port := stack.Ports[name].Value
		if port == 0 {
			continue
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			r.add("port."+name, doctorWarn, fmt.Sprintf("%s is not available: %v", addr, err), "Stop the process using the port or choose a different port before starting the stack.")
			continue
		}
		_ = ln.Close()
		r.add("port."+name, doctorOK, addr+" is available", "")
	}
}

func (r *doctorRunner) checkPortPools(stack *manifest.Stack) {
	if len(stack.Operator.PortPool) == 0 {
		return
	}
	names := make([]string, 0, len(stack.Operator.PortPool))
	for name := range stack.Operator.PortPool {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pool := stack.Operator.PortPool[name]
		if err := validatePortRange(pool.Range); err != nil {
			r.add("port_pool."+name, doctorError, err.Error(), "Use a range like \"8101-8199\".")
			continue
		}
		r.add("port_pool."+name, doctorOK, pool.Range, "")
	}
}

func validatePortRange(value string) error {
	startText, endText, ok := strings.Cut(value, "-")
	if !ok {
		return fmt.Errorf("invalid port range %q", value)
	}
	start, err := strconv.Atoi(strings.TrimSpace(startText))
	if err != nil {
		return fmt.Errorf("invalid port range %q", value)
	}
	end, err := strconv.Atoi(strings.TrimSpace(endText))
	if err != nil {
		return fmt.Errorf("invalid port range %q", value)
	}
	if start < 1 || end > 65535 || start > end {
		return fmt.Errorf("invalid port range %q", value)
	}
	return nil
}

func (r *doctorRunner) checkGitIgnores(ctx context.Context) {
	cwd, err := os.Getwd()
	if err != nil {
		r.add("git.cwd", doctorWarn, err.Error(), "")
		return
	}
	checkDir := cwd
	if host, ok := findTemplateHost(cwd); ok {
		checkDir = host
	}
	repoRoot, err := gitRepoRoot(ctx, checkDir)
	if err != nil {
		r.add("git.repo", doctorWarn, "not inside a git worktree", "Run doctor from the angee-examples repo or a materialized workspace source.")
		return
	}
	r.add("git.repo", doctorOK, displayPath(repoRoot), "")
	for _, path := range []struct {
		name  string
		check string
	}{
		{name: ".angee", check: ".angee/"},
		{name: ".mcp.json", check: ".mcp.json"},
		{name: ".copier-answers.yml", check: ".copier-answers.yml"},
	} {
		if gitCheckIgnore(ctx, repoRoot, path.check) {
			r.add("git.ignore."+path.name, doctorOK, path.name+" is ignored", "")
			continue
		}
		r.add("git.ignore."+path.name, doctorWarn, path.name+" is not ignored in this repo", "Add it to .gitignore before creating runtime state.")
	}
}

func gitRepoRoot(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitCheckIgnore(ctx context.Context, repoRoot string, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "check-ignore", "-q", path)
	err := cmd.Run()
	return err == nil
}

func (r *doctorRunner) checkTemplates() {
	cwd, err := os.Getwd()
	if err != nil {
		r.add("templates", doctorWarn, err.Error(), "")
		return
	}
	host, ok := findTemplateHost(cwd)
	if !ok {
		r.add("templates", doctorWarn, "no .templates directory found nearby", "Run doctor from angee-examples or a workspace that contains an angee-examples worktree.")
		return
	}
	r.add("templates.host", doctorOK, displayPath(host), "")
	templates := []struct {
		name string
		kind string
	}{
		{name: "stacks/dev", kind: "stack"},
		{name: "workspaces/dev-pr", kind: "workspace"},
		{name: "workspaces/dev-pr-multi", kind: "workspace"},
	}
	for _, template := range templates {
		path := filepath.Join(host, ".templates", filepath.FromSlash(template.name))
		if _, err := os.Stat(filepath.Join(path, "copier.yml")); err != nil {
			r.add("template."+template.name, doctorWarn, "missing copier.yml", "Check out or update angee-examples.")
			continue
		}
		if _, err := copierx.ValidateMetadata(path, template.kind); err != nil {
			r.add("template."+template.name, doctorError, err.Error(), "Fix the template _angee metadata.")
			continue
		}
		r.add("template."+template.name, doctorOK, displayPath(path), "")
	}
}

func findTemplateHost(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if hasTemplates(dir) {
			return dir, true
		}
		candidate := filepath.Join(dir, "angee-examples")
		if hasTemplates(candidate) {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func hasTemplates(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".templates"))
	return err == nil && info.IsDir()
}

func summarizeDoctorChecks(checks []doctorCheck) doctorSummary {
	var summary doctorSummary
	for _, check := range checks {
		switch check.Status {
		case doctorOK:
			summary.OK++
		case doctorWarn:
			summary.Warnings++
		case doctorError:
			summary.Errors++
		}
	}
	return summary
}

func writeDoctorReport(w io.Writer, report doctorReport) error {
	if _, err := fmt.Fprintf(w, "angee doctor\nversion: %s\nroot: %s\n\n", report.Version, report.Root); err != nil {
		return err
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(w, "%-5s %-30s %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Detail); err != nil {
			return err
		}
		if check.Hint != "" {
			if _, err := fmt.Fprintf(w, "      hint: %s\n", check.Hint); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(w, "\nsummary: %d ok, %d warning(s), %d error(s)\n", report.Summary.OK, report.Summary.Warnings, report.Summary.Errors)
	return err
}
