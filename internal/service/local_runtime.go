package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fyltr/angee/internal/config"
	"github.com/fyltr/angee/internal/root"
	"github.com/fyltr/angee/internal/state"
)

type localRunRecord struct {
	Name        string    `json:"name"`
	Scope       string    `json:"scope,omitempty"`
	Workspace   string    `json:"workspace,omitempty"`
	Service     string    `json:"service"`
	PID         int       `json:"pid"`
	Command     []string  `json:"command"`
	Cwd         string    `json:"cwd"`
	Fingerprint string    `json:"fingerprint"`
	LogPath     string    `json:"log_path"`
	StartedAt   time.Time `json:"started_at"`
}

type localProcessContext struct {
	Label          string
	Scope          string
	Workspace      string
	RunPrefix      string
	BaseDir        string
	DeclaredLeases map[string]config.PortLeaseSpec
	Leases         map[string]state.PortLease
	Secrets        map[string]state.Secret
	BaseEnv        map[string]string
}

func (c localProcessContext) runName(name string) string {
	if c.RunPrefix == "" {
		return name
	}
	return c.RunPrefix + "-" + name
}

func (p *Platform) startWorkspaceLocalServices(ctx context.Context, workspaceName, workspaceDir string, workspaceCfg *config.AngeeConfig, leases map[string]state.PortLease) ([]string, error) {
	if err := p.cleanupWorkspaceLocalRuns(workspaceName, workspaceCfg); err != nil {
		return nil, err
	}
	secrets, err := state.New(p.Root.Path).LoadSecrets()
	if err != nil {
		return nil, err
	}
	process := localProcessContext{
		Label:          fmt.Sprintf("workspace %q", workspaceName),
		Scope:          workspaceScope(workspaceName),
		Workspace:      workspaceName,
		RunPrefix:      "workspace-" + workspaceName,
		BaseDir:        workspaceDir,
		DeclaredLeases: workspaceCfg.PortLeases,
		Leases:         leases,
		Secrets:        secrets,
		BaseEnv: map[string]string{
			"ANGEE_ROOT":          p.Root.Path,
			"ANGEE_WORKSPACE":     workspaceName,
			"ANGEE_WORKSPACE_DIR": workspaceDir,
			"PYTHONUNBUFFERED":    "1",
		},
	}
	serviceOrder, err := orderedLocalServices(workspaceCfg.Services)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, name := range serviceOrder {
		svc := workspaceCfg.Services[name]
		changedName, err := p.startLocalService(ctx, process, name, svc, nil)
		if err != nil {
			return nil, err
		}
		if changedName != "" {
			changed = append(changed, changedName)
		}
	}
	return changed, nil
}

func (p *Platform) startLocalService(ctx context.Context, process localProcessContext, serviceName string, svc config.ServiceSpec, sink LocalOutputSink) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(svc.Command) == 0 {
		return "", fmt.Errorf("%s service %q: local runtime requires command", process.Label, serviceName)
	}
	runName := process.runName(serviceName)
	cwd := localCwd(process.BaseDir, svc.Cwd)
	command, err := process.resolveCommand(svc.Command)
	if err != nil {
		return "", fmt.Errorf("%s service %q: %w", process.Label, serviceName, err)
	}
	env, err := process.localEnv(svc.Env)
	if err != nil {
		return "", fmt.Errorf("%s service %q: %w", process.Label, serviceName, err)
	}
	fingerprint := localRunFingerprint(command, cwd, env)
	recordPath := p.localRunRecordPath(runName)
	if record, err := readLocalRun(recordPath); err == nil && record.PID > 0 && processAlive(record.PID) {
		if record.Fingerprint == fingerprint && sink == nil {
			return "", nil
		}
		_ = stopProcess(record.PID)
	}

	if err := os.MkdirAll(filepath.Dir(recordPath), 0755); err != nil {
		return "", err
	}
	logPath := p.localRunLogPath(runName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return "", err
	}
	output, closeOutput := localOutputWriter(logFile, sink, serviceName)
	// The process intentionally outlives the HTTP request context; lifecycle is
	// managed by the run record and subsequent dev/update calls.
	cmd := exec.CommandContext(context.Background(), command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		closeOutput()
		_ = logFile.Close()
		return "", fmt.Errorf("%s service %q: start: %w", process.Label, serviceName, err)
	}
	go func() {
		_ = cmd.Wait()
		closeOutput()
		_ = logFile.Close()
	}()
	if sink != nil {
		sink.SystemLine("started [%s] pid=%d", serviceName, cmd.Process.Pid)
	}
	record := localRunRecord{
		Name:        runName,
		Scope:       process.Scope,
		Workspace:   process.Workspace,
		Service:     serviceName,
		PID:         cmd.Process.Pid,
		Command:     append([]string{}, svc.Command...),
		Cwd:         cwd,
		Fingerprint: fingerprint,
		LogPath:     logPath,
		StartedAt:   time.Now().UTC(),
	}
	if err := writeLocalRun(recordPath, record); err != nil {
		_ = stopProcess(cmd.Process.Pid)
		return "", err
	}
	return runName, nil
}

func (p *Platform) stopWorkspaceLocalServices(workspaceName string) error {
	return p.cleanupLocalRuns(workspaceRuntimeName(workspaceName, "*")+".json", map[string]bool{})
}

func (p *Platform) StopStackLocalServices() error {
	return p.cleanupLocalRuns("stack-*.json", map[string]bool{})
}

func (p *Platform) cleanupWorkspaceLocalRuns(workspaceName string, workspaceCfg *config.AngeeConfig) error {
	desired := map[string]bool{}
	for name, svc := range workspaceCfg.Services {
		if svc.Runtime == "local" {
			desired[workspaceRuntimeName(workspaceName, name)] = true
		}
	}
	return p.cleanupLocalRuns(workspaceRuntimeName(workspaceName, "*")+".json", desired)
}

func (p *Platform) runStackLocalJobs(ctx context.Context, cfg *config.AngeeConfig, leases map[string]state.PortLease, secrets map[string]state.Secret, sink LocalOutputSink) ([]string, error) {
	process := p.stackLocalProcessContext(cfg, leases, secrets)
	jobOrder, err := orderedLocalJobs(cfg.Jobs)
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0, len(jobOrder))
	for _, name := range jobOrder {
		if err := p.runLocalJob(ctx, process, name, cfg.Jobs[name], sink); err != nil {
			return nil, err
		}
		changed = append(changed, process.runName("job-"+name))
	}
	return changed, nil
}

func (p *Platform) startStackLocalServices(ctx context.Context, cfg *config.AngeeConfig, leases map[string]state.PortLease, secrets map[string]state.Secret, sink LocalOutputSink) ([]string, error) {
	if err := p.cleanupStackLocalRuns(cfg); err != nil {
		return nil, err
	}
	process := p.stackLocalProcessContext(cfg, leases, secrets)
	serviceOrder, err := orderedLocalServices(cfg.Services)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, name := range serviceOrder {
		changedName, err := p.startLocalService(ctx, process, name, cfg.Services[name], sink)
		if err != nil {
			return nil, err
		}
		if changedName != "" {
			changed = append(changed, changedName)
		}
	}
	return changed, nil
}

func (p *Platform) stackLocalProcessContext(cfg *config.AngeeConfig, leases map[string]state.PortLease, secrets map[string]state.Secret) localProcessContext {
	worktree := filepath.Dir(p.Root.Path)
	return localProcessContext{
		Label:          fmt.Sprintf("stack %q", cfg.Name),
		RunPrefix:      "stack",
		BaseDir:        worktree,
		DeclaredLeases: cfg.PortLeases,
		Leases:         leases,
		Secrets:        secrets,
		BaseEnv: map[string]string{
			"ANGEE_ROOT":       p.Root.Path,
			"ANGEE_STACK":      cfg.Name,
			"ANGEE_WORKTREE":   worktree,
			"PYTHONUNBUFFERED": "1",
		},
	}
}

func (p *Platform) runLocalJob(ctx context.Context, process localProcessContext, jobName string, job config.JobSpec, sink LocalOutputSink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(job.Command) == 0 {
		return fmt.Errorf("%s job %q: local runtime requires command", process.Label, jobName)
	}
	command, err := process.resolveCommand(job.Command)
	if err != nil {
		return fmt.Errorf("%s job %q: %w", process.Label, jobName, err)
	}
	env, err := process.localEnv(job.Env)
	if err != nil {
		return fmt.Errorf("%s job %q: %w", process.Label, jobName, err)
	}
	logPath := p.localRunLogPath(process.runName("job-" + jobName))
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	output, closeOutput := localOutputWriter(logFile, sink, jobName)
	defer closeOutput()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = localCwd(process.BaseDir, job.Cwd)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = output
	cmd.Stderr = output
	if sink != nil {
		sink.SystemLine("running [%s]", jobName)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s job %q: run: %w", process.Label, jobName, err)
	}
	if sink != nil {
		sink.SystemLine("finished [%s]", jobName)
	}
	return nil
}

func localOutputWriter(logFile *os.File, sink LocalOutputSink, name string) (io.Writer, func()) {
	if sink == nil {
		return logFile, func() {}
	}
	writer := sink.Writer(name)
	closeOutput := func() {
		if closer, ok := writer.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return io.MultiWriter(logFile, writer), closeOutput
}

func (p *Platform) cleanupStackLocalRuns(cfg *config.AngeeConfig) error {
	desired := map[string]bool{}
	for name, svc := range cfg.Services {
		if svc.Runtime == "local" {
			desired[stackRuntimeName(name)] = true
		}
	}
	return p.cleanupLocalRuns("stack-*.json", desired)
}

func (p *Platform) cleanupLocalRuns(pattern string, desired map[string]bool) error {
	matches, err := filepath.Glob(filepath.Join(p.Root.Path, root.StateDir, state.RunsDir, pattern))
	if err != nil {
		return err
	}
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		record, err := readLocalRun(path)
		alive := err == nil && record.PID > 0 && processAlive(record.PID)
		if desired[name] && alive {
			continue
		}
		if alive {
			_ = stopProcess(record.PID)
		}
		_ = os.Remove(path)
	}
	return nil
}

func stackRuntimeName(name string) string {
	return "stack-" + name
}

func localCwd(baseDir, cwd string) string {
	if cwd == "" {
		return baseDir
	}
	if filepath.IsAbs(cwd) {
		return cwd
	}
	return filepath.Join(baseDir, filepath.FromSlash(cwd))
}

func (c localProcessContext) localEnv(env map[string]string) ([]string, error) {
	values := make(map[string]string, len(c.BaseEnv)+len(c.DeclaredLeases)+len(env))
	for key, value := range c.BaseEnv {
		values[key] = value
	}
	for name, spec := range c.DeclaredLeases {
		if spec.ExportEnv == "" {
			continue
		}
		if lease := c.Leases[scopeName(c.Scope, name)]; lease.Port > 0 {
			values[spec.ExportEnv] = fmt.Sprintf("%d", lease.Port)
		}
	}
	for key, value := range env {
		resolved, err := c.resolveLocalRefs(value)
		if err != nil {
			return nil, err
		}
		values[key] = resolved
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out, nil
}

func (c localProcessContext) resolveCommand(command []string) ([]string, error) {
	out := make([]string, len(command))
	for i, value := range command {
		resolved, err := c.resolveLocalRefs(value)
		if err != nil {
			return nil, err
		}
		out[i] = resolved
	}
	return out, nil
}

func (c localProcessContext) resolveLocalRefs(value string) (string, error) {
	resolved, err := resolveLocalPortRefs(value, c.Scope, c.Leases)
	if err != nil {
		return "", err
	}
	return resolveLocalSecretRefs(resolved, c.Scope, c.Secrets)
}

func resolveLocalPortRefs(value, scope string, leases map[string]state.PortLease) (string, error) {
	for {
		start := strings.Index(value, "${ports.")
		if start == -1 {
			return value, nil
		}
		end := strings.Index(value[start:], "}")
		if end == -1 {
			return "", fmt.Errorf("unterminated port reference in %q", value)
		}
		end += start
		name := value[start+len("${ports.") : end]
		lease := leases[scopeName(scope, name)]
		if lease.Port == 0 {
			return "", fmt.Errorf("port lease %q is not resolved", name)
		}
		value = value[:start] + fmt.Sprintf("%d", lease.Port) + value[end+1:]
	}
}

func resolveLocalSecretRefs(value, scope string, secrets map[string]state.Secret) (string, error) {
	for {
		start := strings.Index(value, "${secret:")
		if start == -1 {
			return value, nil
		}
		end := strings.Index(value[start:], "}")
		if end == -1 {
			return "", fmt.Errorf("unterminated secret reference in %q", value)
		}
		end += start
		name := value[start+len("${secret:") : end]
		secret := secrets[scopeName(scope, name)]
		if secret.Value == "" {
			secret = secrets[name]
		}
		if secret.Value == "" {
			return "", fmt.Errorf("secret %q is not resolved", name)
		}
		value = value[:start] + secret.Value + value[end+1:]
	}
}

func localRunFingerprint(command []string, cwd string, env []string) string {
	sum := sha256.Sum256([]byte(strings.Join(command, "\x00") + "\x00" + cwd + "\x00" + strings.Join(env, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (p *Platform) localRunRecordPath(name string) string {
	return filepath.Join(p.Root.Path, root.StateDir, state.RunsDir, name+".json")
}

func (p *Platform) localRunLogPath(name string) string {
	return filepath.Join(p.Root.Path, root.StateDir, state.RunsDir, name+".log")
}

func readLocalRun(path string) (localRunRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return localRunRecord{}, err
	}
	var record localRunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return localRunRecord{}, err
	}
	return record, nil
}

func writeLocalRun(path string, record localRunRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func stopProcess(pid int) error {
	if pid <= 0 || !processAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(os.Interrupt)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return proc.Kill()
}

func orderedLocalServices(services map[string]config.ServiceSpec) ([]string, error) {
	pending := map[string]bool{}
	for name, svc := range services {
		if svc.Runtime == "local" {
			pending[name] = true
		}
	}
	var ordered []string
	for len(pending) > 0 {
		progressed := false
		for _, name := range sortedBoolKeys(pending) {
			ready := true
			for _, dep := range services[name].After {
				if pending[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			ordered = append(ordered, name)
			delete(pending, name)
			progressed = true
		}
		if !progressed {
			return nil, fmt.Errorf("local service dependency cycle: %s", strings.Join(sortedBoolKeys(pending), ", "))
		}
	}
	return ordered, nil
}

func orderedLocalJobs(jobs map[string]config.JobSpec) ([]string, error) {
	pending := map[string]bool{}
	for name, job := range jobs {
		if isLocalJob(job) {
			pending[name] = true
		}
	}
	var ordered []string
	for len(pending) > 0 {
		progressed := false
		for _, name := range sortedBoolKeys(pending) {
			ready := true
			for _, dep := range jobs[name].After {
				if pending[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			ordered = append(ordered, name)
			delete(pending, name)
			progressed = true
		}
		if !progressed {
			return nil, fmt.Errorf("local job dependency cycle: %s", strings.Join(sortedBoolKeys(pending), ", "))
		}
	}
	return ordered, nil
}

func isLocalJob(job config.JobSpec) bool {
	if len(job.Command) == 0 {
		return false
	}
	return job.Runtime == "local" || job.Kind == "process" || (job.Runtime == "" && job.Kind == "")
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
