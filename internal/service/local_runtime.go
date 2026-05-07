package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Workspace   string    `json:"workspace"`
	Service     string    `json:"service"`
	PID         int       `json:"pid"`
	Command     []string  `json:"command"`
	Cwd         string    `json:"cwd"`
	Fingerprint string    `json:"fingerprint"`
	LogPath     string    `json:"log_path"`
	StartedAt   time.Time `json:"started_at"`
}

func (p *Platform) startWorkspaceLocalServices(ctx context.Context, workspaceName, workspaceDir string, workspaceCfg *config.AngeeConfig, leases map[string]state.PortLease) ([]string, error) {
	if err := p.cleanupWorkspaceLocalRuns(workspaceName, workspaceCfg); err != nil {
		return nil, err
	}
	secrets, err := state.New(p.Root.Path).LoadSecrets()
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, name := range sortedConfigKeys(workspaceCfg.Services) {
		svc := workspaceCfg.Services[name]
		if svc.Runtime != "local" {
			continue
		}
		changedName, err := p.startWorkspaceLocalService(ctx, workspaceName, workspaceDir, name, svc, workspaceCfg.PortLeases, leases, secrets)
		if err != nil {
			return nil, err
		}
		if changedName != "" {
			changed = append(changed, changedName)
		}
	}
	return changed, nil
}

func (p *Platform) startWorkspaceLocalService(ctx context.Context, workspaceName, workspaceDir, serviceName string, svc config.ServiceSpec, declaredLeases map[string]config.PortLeaseSpec, leases map[string]state.PortLease, secrets map[string]state.Secret) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(svc.Command) == 0 {
		return "", fmt.Errorf("workspace %q service %q: local runtime requires command", workspaceName, serviceName)
	}
	runName := workspaceRuntimeName(workspaceName, serviceName)
	cwd := workspaceServiceCwd(workspaceDir, svc)
	env, err := p.workspaceLocalEnv(workspaceName, workspaceDir, svc, declaredLeases, leases, secrets)
	if err != nil {
		return "", fmt.Errorf("workspace %q service %q: %w", workspaceName, serviceName, err)
	}
	fingerprint := localRunFingerprint(svc.Command, cwd, env)
	recordPath := p.localRunRecordPath(runName)
	if record, err := readLocalRun(recordPath); err == nil && record.PID > 0 && processAlive(record.PID) {
		if record.Fingerprint == fingerprint {
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
	defer logFile.Close()
	// The process intentionally outlives the HTTP request context; lifecycle is
	// managed by the run record and subsequent workspace dev/update calls.
	cmd := exec.CommandContext(context.Background(), svc.Command[0], svc.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("workspace %q service %q: start: %w", workspaceName, serviceName, err)
	}
	go func() { _ = cmd.Wait() }()
	record := localRunRecord{
		Name:        runName,
		Workspace:   workspaceName,
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
	pattern := filepath.Join(p.Root.Path, root.StateDir, state.RunsDir, workspaceRuntimeName(workspaceName, "*")+".json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	for _, path := range matches {
		record, err := readLocalRun(path)
		if err == nil && record.PID > 0 {
			_ = stopProcess(record.PID)
		}
		_ = os.Remove(path)
	}
	return nil
}

func (p *Platform) cleanupWorkspaceLocalRuns(workspaceName string, workspaceCfg *config.AngeeConfig) error {
	desired := map[string]bool{}
	for name, svc := range workspaceCfg.Services {
		if svc.Runtime == "local" {
			desired[workspaceRuntimeName(workspaceName, name)] = true
		}
	}
	pattern := filepath.Join(p.Root.Path, root.StateDir, state.RunsDir, workspaceRuntimeName(workspaceName, "*")+".json")
	matches, err := filepath.Glob(pattern)
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

func workspaceServiceCwd(workspaceDir string, svc config.ServiceSpec) string {
	if svc.Cwd == "" {
		return workspaceDir
	}
	if filepath.IsAbs(svc.Cwd) {
		return svc.Cwd
	}
	return filepath.Join(workspaceDir, filepath.FromSlash(svc.Cwd))
}

func (p *Platform) workspaceLocalEnv(workspaceName, workspaceDir string, svc config.ServiceSpec, declaredLeases map[string]config.PortLeaseSpec, leases map[string]state.PortLease, secrets map[string]state.Secret) ([]string, error) {
	values := map[string]string{
		"ANGEE_ROOT":          p.Root.Path,
		"ANGEE_WORKSPACE":     workspaceName,
		"ANGEE_WORKSPACE_DIR": workspaceDir,
	}
	for name, spec := range declaredLeases {
		if spec.ExportEnv == "" {
			continue
		}
		if lease := leases[scopeName(workspaceScope(workspaceName), name)]; lease.Port > 0 {
			values[spec.ExportEnv] = fmt.Sprintf("%d", lease.Port)
		}
	}
	for key, value := range svc.Env {
		resolved, err := resolveLocalSecretRefs(value, workspaceName, secrets)
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

func resolveLocalSecretRefs(value, workspaceName string, secrets map[string]state.Secret) (string, error) {
	scope := workspaceScope(workspaceName)
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

func sortedConfigKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
