package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

type EnvLookup func(string) (string, bool)

func ResolveDeclarations(ctx context.Context, backend Backend, declarations map[string]manifest.Secret, lookup EnvLookup) (map[string]string, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	resolved := make(map[string]string, len(declarations))
	for name, spec := range declarations {
		value, ok, err := backend.Get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("get secret %q: %w", name, err)
		}
		if !ok && spec.Import != "" {
			importValue, err := importSecret(spec.Import, lookup)
			if err != nil {
				return nil, fmt.Errorf("import secret %q: %w", name, err)
			}
			value = importValue
			ok = true
			if err := backend.Set(ctx, name, value); err != nil {
				return nil, fmt.Errorf("persist imported secret %q: %w", name, err)
			}
		}
		if !ok && spec.Generated {
			length := spec.Length
			if length == 0 {
				length = 32
			}
			generated, err := Generate(length)
			if err != nil {
				return nil, fmt.Errorf("generate secret %q: %w", name, err)
			}
			value = generated
			ok = true
			if err := backend.Set(ctx, name, value); err != nil {
				return nil, fmt.Errorf("persist generated secret %q: %w", name, err)
			}
		}
		if !ok && spec.Required {
			return nil, fmt.Errorf("required secret %q is missing", name)
		}
		if ok {
			resolved[name] = value
		}
	}
	return resolved, nil
}

func Generate(length int) (string, error) {
	if length < 1 {
		return "", fmt.Errorf("secret length must be positive")
	}
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) < length {
		return Generate(length)
	}
	return encoded[:length], nil
}

func importSecret(ref string, lookup EnvLookup) (string, error) {
	prefix, key, ok := strings.Cut(ref, ":")
	if !ok || prefix != "env" || key == "" {
		return "", fmt.Errorf("unsupported import reference %q", ref)
	}
	value, ok := lookup(key)
	if !ok {
		return "", fmt.Errorf("environment variable %q is not set", key)
	}
	return value, nil
}
