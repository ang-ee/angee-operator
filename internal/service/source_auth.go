package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gitx "github.com/ang-ee/angee-operator/internal/git"
	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/secrets"
	"github.com/ang-ee/angee-operator/internal/substitute"
)

// A keyed source is a git source that names its own SSH key
// (`auth.ssh_key_secret`). The operator reaches its remotes with that key and
// no other, from the source's cache and from every workspace slot cut from
// it: two private repositories on one host can then each have a deploy key of
// their own, with no host alias in the user's ssh config.
//
// The key is a stack secret. ssh reads a key from a file, so the operator
// keeps a copy under the stack root, in sourceSSHDir, which it writes from
// the secret (ensureSourceKey) before it reaches the source's remote. The
// stack root is at the same path for the CLI on the host and for the
// operator in its container, so both name the same file.

// sourceSSHDir is where the stack keeps the SSH key files of its keyed
// sources, each named after its source, and the known_hosts file their
// connections record host keys in.
func (p *Platform) sourceSSHDir() string {
	return filepath.Join(p.root, "run", "ssh")
}

// sourceKeyFile is the file the SSH key of keyed source name is written to.
// A name that is not a plain file name is escaped, as a URL path segment is.
func (p *Platform) sourceKeyFile(name string) string {
	return filepath.Join(p.sourceSSHDir(), url.PathEscape(name))
}

// sourceKnownHostsFile is the known_hosts file of the stack's keyed sources.
// manifest.ValidateSource refuses a keyed source named after it.
func (p *Platform) sourceKnownHostsFile() string {
	return filepath.Join(p.sourceSSHDir(), manifest.SourceKnownHostsName)
}

// sourceSSHCommand is the GIT_SSH_COMMAND every git and jj command on source
// name runs with: for a keyed source, the one that pins its key
// (git.SSHKeyCommand), and "" for any other, which keeps the ssh agent, keys
// and credential helpers of the operator's environment. It only names the
// key file; ensureSourceKey writes it.
func (p *Platform) sourceSSHCommand(name string, source manifest.Source) string {
	if source.Auth.SSHKeySecret == "" {
		return ""
	}
	return gitx.SSHKeyCommand(p.sourceKeyFile(name), p.sourceKnownHostsFile())
}

// sshCommandEnv is the git.Client.Env that sets GIT_SSH_COMMAND to command,
// and none for an empty command.
func sshCommandEnv(command string) []string {
	if command == "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=" + command}
}

// sourceGitClient is gitClient for the cache and checkouts of source name:
// its commands run with sourceSSHCommand.
func (p *Platform) sourceGitClient(name string, source manifest.Source) gitx.Client {
	client := p.gitClient()
	client.Env = sshCommandEnv(p.sourceSSHCommand(name, source))
	return client
}

// ensureSourceKeys is ensureSourceKey for every source of stack, in name
// order, stopping at the first that fails, as a bring-up runs it before it
// reaches any source's remote. It first removes the key files no source of
// the stack has any more (pruneSourceKeys).
func (p *Platform) ensureSourceKeys(ctx context.Context, stack *manifest.Stack) error {
	p.pruneSourceKeys(ctx, stack)
	for _, name := range sortedKeys(stack.Sources) {
		if err := p.ensureSourceKey(ctx, stack, name); err != nil {
			return err
		}
	}
	return nil
}

// pruneSourceKeys removes from sourceSSHDir what no keyed source of stack
// accounts for: the key file of a source that was removed or renamed, or
// that names no key any more, so no private key outlives its use there. The
// known-hosts file, and whatever ssh keeps beside it under its name, stay. A
// staging file writeSourceKey left is removed once it is an hour old, so one
// another process is writing now is not. It is best effort: what cannot be
// removed is warned of.
func (p *Platform) pruneSourceKeys(ctx context.Context, stack *manifest.Stack) {
	dir := p.sourceSSHDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for name, source := range stack.Sources {
		if source.Auth.SSHKeySecret != "" {
			keep[filepath.Base(p.sourceKeyFile(name))] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] || strings.HasPrefix(name, manifest.SourceKnownHostsName) || !entry.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(name, ".") {
			if info, err := entry.Info(); err != nil || time.Since(info.ModTime()) < time.Hour {
				continue
			}
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logctx.From(ctx).Warn(fmt.Sprintf("could not remove the unused SSH key file %s: %v", filepath.Join(dir, name), err))
		}
	}
}

// ensureSourceKey writes the key file of keyed source name from its secret,
// where sourceSSHCommand points ssh at it; for any other source it does
// nothing. The callers are the operations that reach a source's remote, each
// before it does: a bring-up, before it clones or fetches any cache; the
// source verbs fetch, pull and push; the creation and repair of a workspace,
// before any slot is cut; and the slot verbs fetch, pull, push, publish and
// sync-base. A read (status, diff, the commit list) runs no ssh, and needs
// no key. The file is rewritten only when the secret's value differs from
// it, so a key set again with `angee secret set` is used from the next such
// operation on.
//
// A key that is not set is a precondition failure (CauseSourceKeyMissing)
// that names the source and the secret, before git is run; a key file an
// earlier value left is removed.
func (p *Platform) ensureSourceKey(ctx context.Context, stack *manifest.Stack, name string) error {
	source, ok := stack.Sources[name]
	secret := source.Auth.SSHKeySecret
	if !ok || secret == "" {
		return nil
	}
	key, err := p.sourceKey(ctx, stack, secret)
	if err != nil {
		return fmt.Errorf("source %q: read its SSH key from secret %q: %w", name, secret, err)
	}
	if strings.TrimSpace(key) == "" {
		_ = os.Remove(p.sourceKeyFile(name))
		hint := fmt.Sprintf("Set it to the private key with `angee secret set %s --stdin < <key file>`.", secret)
		return &OperationError{
			Code:    CodePreconditionFailed,
			Cause:   CauseSourceKeyMissing,
			Source:  name,
			Hint:    hint,
			Summary: fmt.Sprintf("source %q authenticates with the SSH key in secret %q, which is not set. %s", name, secret, hint),
		}
	}
	if err := writeSourceKey(p.sourceSSHDir(), p.sourceKeyFile(name), key); err != nil {
		return fmt.Errorf("source %q: write its SSH key file: %w", name, err)
	}
	return nil
}

// sourceKey reads the SSH key in secret, "" when it is not set. A secret the
// stack declares is resolved as the stack's secrets are, so one imported from
// the environment is taken from it and kept; the manifest refuses one that
// would be generated. Whether a missing key is an error is the caller's to
// say, for the source it is the key of.
func (p *Platform) sourceKey(ctx context.Context, stack *manifest.Stack, secret string) (string, error) {
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return "", err
	}
	spec, declared := stack.Secrets[secret]
	if !declared {
		value, _, err := backend.Get(ctx, secret)
		if err == nil {
			logctx.RegisterSecrets(value)
		}
		return value, err
	}
	spec.Required = false
	resolved, err := secrets.ResolveDeclarations(ctx, backend, map[string]manifest.Secret{secret: spec}, os.LookupEnv)
	if err != nil {
		return "", err
	}
	return resolved[secret], nil
}

// writeSourceKey makes file, in dir, hold key, readable by its owner only:
// ssh refuses a key file others can read. dir is made private too. A key
// without a final newline gets one, since ssh refuses a key file that ends
// mid-line, and a secret set from standard input loses its last newline. The
// file is replaced, not written into, and only when its content differs, so
// a command reading it never sees half a key.
func writeSourceKey(dir, file, key string) error {
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	// The directory may have been made under another umask, or by hand.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() {
		if current, err := os.ReadFile(file); err == nil && string(current) == key {
			if info.Mode().Perm() == 0o600 {
				return nil
			}
			return os.Chmod(file, 0o600)
		}
	}
	// os.CreateTemp makes the file readable by its owner only.
	staged, err := os.CreateTemp(dir, "."+filepath.Base(file)+".new-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged.Name()) }()
	if _, err := staged.WriteString(key); err != nil {
		_ = staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(staged.Name(), file)
}
