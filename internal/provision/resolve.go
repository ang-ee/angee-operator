// Package provision implements operator-owned provisioning steps.
package provision

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fyltr/angee/internal/config"
	"github.com/fyltr/angee/internal/state"
)

const defaultSecretCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// ResolvePortLeases preserves existing leases, applies explicit overrides,
// allocates missing leases, and persists the resulting state.
func ResolvePortLeases(store *state.Store, declared map[string]config.PortLeaseSpec, overrides map[string]int, owner string) (map[string]state.PortLease, error) {
	for name, port := range overrides {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("port override %q does not match a declared port lease", name)
		}
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("port override %q has invalid port %d", name, port)
		}
	}

	leases, err := store.LoadPortLeases()
	if err != nil {
		return nil, err
	}
	used := map[int]string{}
	for name, lease := range leases {
		if lease.Port > 0 {
			used[lease.Port] = name
		}
	}

	now := time.Now().UTC()
	for _, name := range sortedKeys(declared) {
		spec := declared[name]
		port, overridden := overrides[name]
		if !overridden {
			if existing, ok := leases[name]; ok && existing.Port > 0 {
				continue
			}
			port = spec.Default
		}
		if port == 0 || usedByOther(used, port, name) || !portAvailable(port) {
			allocated, err := findAvailablePort(used, spec.Default)
			if err != nil {
				return nil, fmt.Errorf("allocating port lease %q: %w", name, err)
			}
			port = allocated
		}
		used[port] = name
		leases[name] = state.PortLease{Name: name, Port: port, Band: spec.Band, Owner: owner, UpdatedAt: now}
	}

	if err := store.SavePortLeases(leases); err != nil {
		return nil, err
	}
	return leases, nil
}

// ResolveSecrets preserves existing generated values, applies supplied values,
// generates requested secrets, and persists secret state.
func ResolveSecrets(store *state.Store, declared map[string]config.SecretSpec, supplied map[string]string) (map[string]state.Secret, error) {
	for name := range supplied {
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("secret %q does not match a declared secret", name)
		}
	}

	secrets, err := store.LoadSecrets()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, name := range sortedKeys(declared) {
		spec := declared[name]
		if raw, ok := supplied[name]; ok {
			value, source, err := suppliedSecretValue(raw)
			if err != nil {
				return nil, fmt.Errorf("secret %q: %w", name, err)
			}
			secrets[name] = state.Secret{Name: name, Value: value, Source: source, UpdatedAt: now}
			continue
		}
		if existing, ok := secrets[name]; ok && existing.Value != "" {
			continue
		}
		if spec.Default != "" {
			secrets[name] = state.Secret{Name: name, Value: spec.Default, Source: "default", UpdatedAt: now}
			continue
		}
		if spec.Generated {
			length := spec.Length
			if length <= 0 {
				length = 32
			}
			charset := spec.Charset
			if charset == "" {
				charset = defaultSecretCharset
			}
			value, err := randomString(length, charset)
			if err != nil {
				return nil, fmt.Errorf("generating secret %q: %w", name, err)
			}
			secrets[name] = state.Secret{Name: name, Value: value, Source: "generated", UpdatedAt: now}
			continue
		}
		if spec.Required {
			return nil, fmt.Errorf("required secret %q was not supplied", name)
		}
	}

	if err := store.SaveSecrets(secrets); err != nil {
		return nil, err
	}
	return secrets, nil
}

func suppliedSecretValue(raw string) (value string, source string, err error) {
	if strings.HasPrefix(raw, "env:") {
		name := strings.TrimPrefix(raw, "env:")
		if name == "" {
			return "", "", fmt.Errorf("env source is missing a variable name")
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			return "", "", fmt.Errorf("environment variable %s is not set", name)
		}
		return value, "env:" + name, nil
	}
	return raw, "supplied", nil
}

func randomString(length int, charset string) (string, error) {
	if length <= 0 {
		return "", nil
	}
	if charset == "" {
		return "", fmt.Errorf("charset is empty")
	}
	out := make([]byte, length)
	max := big.NewInt(int64(len(charset)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = charset[n.Int64()]
	}
	return string(out), nil
}

func findAvailablePort(used map[int]string, preferred int) (int, error) {
	if preferred > 0 && !usedByOther(used, preferred, "") && portAvailable(preferred) {
		return preferred, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port <= 0 {
		return 0, fmt.Errorf("could not determine allocated port")
	}
	if usedByOther(used, addr.Port, "") {
		return 0, fmt.Errorf("allocated port %d is already used", addr.Port)
	}
	return addr.Port, nil
}

func portAvailable(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func usedByOther(used map[int]string, port int, name string) bool {
	owner, ok := used[port]
	return ok && owner != name
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
