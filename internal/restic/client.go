package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/toolinstall"
)

const maxErrorBytes = 4 << 10

var (
	defaultMetadataTimeout   = 5 * time.Minute
	defaultMetadataRetryLock = 4 * time.Minute
	dataRetryLock            = 5 * time.Minute
)

var resticTool = toolinstall.Tool{Name: "restic", InstallPackage: InstallPackage}

// Client runs typed restic operations. Prompt enables the pinned install
// offer when PromptIn is interactive; it does not make any operation itself
// interactive because repository passwords always use inherited fds.
type Client struct {
	Runner    Runner
	Binary    string
	Prompt    bool
	PromptIn  io.Reader
	PromptOut io.Writer
	PromptErr io.Writer

	lookupPath        func(string) (string, error)
	goBinPath         func(context.Context) (string, error)
	installRestic     func(context.Context, io.Writer, io.Writer) error
	resolveMu         sync.Mutex
	resolved          string
	resolving         chan struct{}
	metadataTimeout   time.Duration
	metadataRetryLock time.Duration
}

// New returns a client backed by real restic processes.
func New() *Client {
	return &Client{Runner: ExecRunner{}}
}

// ResolveBinary returns the restic executable the client will run. It searches
// PATH before $(go env GOPATH)/bin and may offer the pinned install command.
func (c *Client) ResolveBinary(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("nil restic client")
	}
	if c.Binary != "" {
		return c.Binary, nil
	}
	for {
		c.resolveMu.Lock()
		if c.resolved != "" {
			resolved := c.resolved
			c.resolveMu.Unlock()
			return resolved, nil
		}
		if c.resolving != nil {
			done := c.resolving
			c.resolveMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		c.resolving = make(chan struct{})
		break
	}
	done := c.resolving
	c.resolveMu.Unlock()

	stdin := io.Reader(os.Stdin)
	var stdout, stderr io.Writer = os.Stdout, os.Stderr
	explicit := false
	if c.PromptIn != nil {
		stdin = c.PromptIn
		explicit = true
	}
	if c.PromptOut != nil {
		stdout = c.PromptOut
	}
	if c.PromptErr != nil {
		stderr = c.PromptErr
	}
	resolved, err := resticTool.Resolve(ctx, toolinstall.ResolveOptions{
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		Prompt:        c.Prompt,
		InputExplicit: explicit,
		LookupPath:    c.lookupPath,
		GoBinPath:     c.goBinPath,
		Install:       c.installRestic,
	})
	c.resolveMu.Lock()
	if err == nil {
		c.resolved = resolved
	}
	c.resolving = nil
	close(done)
	c.resolveMu.Unlock()
	return resolved, err
}

type invocation struct {
	repository *Repository
	source     *Repository
	dir        string
	args       []string
	stdin      io.Reader
	stdout     io.Writer
	metadata   bool
	retryLock  bool
	noLock     bool
	passwords  []string
}

func (c *Client) run(ctx context.Context, call invocation) (CommandResult, error) {
	var result CommandResult
	if c == nil {
		return result, adapterError(Unavailable, -1, "restic client is nil", "Create a restic client before running an operation.", nil)
	}
	if call.metadata {
		timeout := c.metadataTimeout
		if timeout == 0 {
			timeout = defaultMetadataTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	binary, err := c.ResolveBinary(ctx)
	if err != nil {
		return result, adapterError(Unavailable, -1, "restic executable is unavailable", "Install it with `go install "+InstallPackage+"` or add it to PATH.", err)
	}

	args := []string{"--json"}
	if call.noLock {
		args = append(args, "--no-lock")
	}
	if call.retryLock {
		window := dataRetryLock
		if call.metadata {
			window = c.metadataRetryLock
			if window == 0 {
				window = defaultMetadataRetryLock
			}
		}
		args = append(args, "--retry-lock", window.String())
	}
	args = append(args, call.args...)
	env, passwords, secrets, err := invocationEnvironment(call)
	if err != nil {
		return result, err
	}
	if call.passwords != nil {
		passwords = append(passwords, call.passwords...)
		secrets = append(secrets, call.passwords...)
		logctx.RegisterSecrets(call.passwords...)
	}
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	result, runErr := runner.Run(ctx, Command{
		Name:      binary,
		Dir:       call.dir,
		Args:      args,
		Env:       env,
		Stdin:     call.stdin,
		Stdout:    call.stdout,
		Passwords: passwords,
		Secrets:   secrets,
	})
	result.secrets = append([]string(nil), secrets...)
	if runErr == nil {
		return result, nil
	}
	return result, commandError(ctx, runErr, result.Stderr, secrets)
}

func invocationEnvironment(call invocation) ([]string, []string, []string, error) {
	if call.repository == nil {
		return allowedEnvironment(nil), nil, nil, nil
	}
	if err := validateRepository(*call.repository, true); err != nil {
		return nil, nil, nil, err
	}
	if call.source != nil {
		if err := validateRepository(*call.source, false); err != nil {
			return nil, nil, nil, fmt.Errorf("source repository: %w", err)
		}
	}
	var sourceEnv []string
	if call.source != nil {
		sourceEnv = call.source.Env
	}
	backendEnv, backendSecrets, err := mergeBackendEnvironment(call.repository.Env, sourceEnv)
	if err != nil {
		return nil, nil, nil, err
	}

	extras := []string{
		"RESTIC_REPOSITORY=" + call.repository.URL,
		"RESTIC_PASSWORD_FILE=/dev/fd/3",
		"RESTIC_CACHE_DIR=" + call.repository.CacheDir,
	}
	passwords := []string{call.repository.Password}
	secrets, registered := repositorySecretValues(*call.repository)
	if call.source != nil {
		extras = append(extras, "RESTIC_FROM_REPOSITORY="+call.source.URL)
		passwords = append(passwords, call.source.Password)
		sourceSecrets, sourceRegistered := repositorySecretValues(*call.source)
		secrets = append(secrets, sourceSecrets...)
		registered = append(registered, sourceRegistered...)
	}
	extras = append(extras, backendEnv...)
	secrets = append(secrets, backendSecrets...)
	registered = append(registered, backendSecrets...)
	logctx.RegisterSecrets(registered...)
	return allowedEnvironment(extras), passwords, secrets, nil
}

func validateRepository(repository Repository, requireCache bool) error {
	switch {
	case repository.URL == "":
		return adapterError(Failure, -1, "restic repository is empty", "Configure a repository URL or path.", nil)
	case repository.Password == "":
		return adapterError(Failure, -1, "restic repository password is empty", "Configure a non-empty repository password.", nil)
	case strings.TrimSpace(repository.Password) != repository.Password || strings.ContainsAny(repository.Password, "\r\n"):
		return adapterError(Failure, -1, "restic repository password has leading or trailing whitespace", "Configure a password without surrounding whitespace or line breaks.", nil)
	case requireCache && repository.CacheDir == "":
		return adapterError(Failure, -1, "restic cache directory is empty", "Pass a per-stack RESTIC_CACHE_DIR through Repository.CacheDir.", nil)
	default:
		return nil
	}
}

func mergeBackendEnvironment(destination []string, source []string) ([]string, []string, error) {
	type valueEntry struct {
		value string
	}
	seen := make(map[string]valueEntry, len(destination)+len(source))
	var merged, secrets []string
	for _, entries := range [][]string{destination, source} {
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
				return nil, nil, adapterError(Failure, -1, "invalid restic backend environment entry", "Pass backend credentials as KEY=value entries.", nil)
			}
			upper := strings.ToUpper(key)
			if strings.HasPrefix(upper, "RESTIC_") {
				return nil, nil, adapterError(Failure, -1, "backend environment must not set "+key, "Use the typed repository and cache options for RESTIC_* settings.", nil)
			}
			if prior, exists := seen[upper]; exists {
				if prior.value != value {
					return nil, nil, adapterError(Failure, -1, "conflicting backend environment value for "+key, "Pass one value for each destination/source backend environment key.", nil)
				}
				continue
			}
			seen[upper] = valueEntry{value: value}
			merged = append(merged, entry)
			secrets = append(secrets, value)
		}
	}
	return merged, secrets, nil
}

func allowedEnvironment(extras []string) []string {
	overrides := make(map[string]struct{}, len(extras))
	for _, entry := range extras {
		if key, _, ok := strings.Cut(entry, "="); ok {
			overrides[strings.ToUpper(key)] = struct{}{}
		}
	}
	env := make([]string, 0, len(os.Environ())+len(extras))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if !ok || !allowedAmbientKey(upper) {
			continue
		}
		if _, overridden := overrides[upper]; overridden {
			continue
		}
		env = append(env, entry)
	}
	return append(env, extras...)
}

func allowedAmbientKey(key string) bool {
	if strings.HasPrefix(key, "LC_") {
		return true
	}
	switch key {
	case "PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TZ", "LANG",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"SSH_AUTH_SOCK", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "SYSTEMROOT",
		"USERPROFILE", "LOCALAPPDATA", "APPDATA", "TEMP", "TMP", "PATHEXT", "COMSPEC":
		return true
	default:
		return false
	}
}

func commandError(ctx context.Context, runErr error, stderr []byte, secrets []string) *Error {
	var adapterErr *Error
	if errors.As(runErr, &adapterErr) {
		return adapterErr
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code = exitErr.ExitCode()
	}
	messageCode, message := exitErrorMessage(stderr)
	if code < 0 && messageCode != 0 {
		code = messageCode
	}
	if message == "" {
		message = tail(strings.TrimSpace(string(stderr)), maxErrorBytes)
	}
	if message == "" {
		message = runErr.Error()
	}
	kind := kindForExitCode(code)
	if code == 1 && strings.Contains(strings.ToLower(message), "failed to find snapshot") {
		kind = NotFound
	}
	message = redactSecrets(message, secrets)
	if ctx.Err() != nil {
		kind = Interrupted
		message = ctx.Err().Error()
		runErr = errors.Join(runErr, ctx.Err())
	}
	return adapterError(kind, code, message, hintForKind(kind), runErr)
}

func adapterError(kind Kind, code int, message string, hint string, err error) *Error {
	return &Error{
		Kind:     kind,
		ExitCode: code,
		Message:  logctx.RedactText(message),
		Hint:     logctx.RedactText(hint),
		err:      err,
	}
}

func kindForExitCode(code int) Kind {
	switch code {
	case 10:
		return NotFound
	case 11:
		return Locked
	case 12:
		return WrongPassword
	case 3:
		return WriteFailure
	case 130:
		return Interrupted
	default:
		return Failure
	}
}

func hintForKind(kind Kind) string {
	switch kind {
	case NotFound:
		return "Check the repository and snapshot ID, or initialize the repository first."
	case Locked:
		return "Wait for the other operation, or run restic unlock when its lock is stale."
	case WrongPassword:
		return "Check the repository password."
	case WriteFailure:
		return "Fix the unreadable input and forget any incomplete snapshot before retrying."
	case Interrupted:
		return "Retry the operation when the caller is ready."
	case Unavailable:
		return "Install the supported restic release and retry."
	default:
		return "Inspect restic's redacted message and retry with an appropriate caller deadline."
	}
}

type exitMessage struct {
	MessageType string `json:"message_type"`
	Code        int    `json:"code"`
	Message     string `json:"message"`
}

func exitErrorMessage(stderr []byte) (int, string) {
	var found exitMessage
	for _, line := range strings.Split(string(stderr), "\n") {
		var candidate exitMessage
		if json.Unmarshal([]byte(line), &candidate) == nil && candidate.MessageType == "exit_error" {
			found = candidate
		}
	}
	return found.Code, found.Message
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return "… " + text[len(text)-limit:]
}

func repositorySecretValues(repository Repository) ([]string, []string) {
	username, urlValues, urlSecrets := logctx.URLCredentials(repository.URL)
	perCall := []string{repository.URL, logctx.RedactURL(repository.URL), username, repository.Password}
	perCall = append(perCall, urlValues...)
	processWide := append([]string{repository.Password}, urlSecrets...)
	return perCall, processWide
}

func redactSecretBytes(data []byte, secrets []string) []byte {
	if len(data) == 0 {
		return data
	}
	return []byte(redactSecrets(string(data), secrets))
}

func redactSecrets(text string, secrets []string) string {
	ordered := append([]string(nil), secrets...)
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range ordered {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

var semanticVersion = regexp.MustCompile(`(?:^|\s|v)(\d+)\.(\d+)\.(\d+)`)

// VersionAtLeast reports whether text contains a semantic version at or above
// floor. It accepts `restic 0.19.1` as printed by `restic version`.
func VersionAtLeast(text string, floor string) bool {
	have, ok := parseVersion(text)
	if !ok {
		return false
	}
	want, ok := parseVersion(floor)
	return ok && slices.Compare(have, want) >= 0
}

func parseVersion(text string) ([]int, bool) {
	match := semanticVersion.FindStringSubmatch(text)
	if match == nil {
		return nil, false
	}
	parts := make([]int, 3)
	for i := range parts {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return nil, false
		}
		parts[i] = value
	}
	return parts, true
}
