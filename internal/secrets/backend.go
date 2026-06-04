package secrets

import (
	"context"
	"fmt"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

type Backend interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context) ([]string, error)
}

func FromManifest(root string, config manifest.SecretsBackend, keyMapper func(string) string) (Backend, error) {
	switch config.Type {
	case "", "env-file":
		path := config.Path
		if path == "" {
			path = ".env"
		}
		return NewEnvFileBackend(manifest.ResolvePath(root, path), WithKeyMapper(keyMapper)), nil
	case "openbao":
		return NewOpenBaoBackend(OpenBaoConfig{
			Address: config.Address,
			Mount:   config.Mount,
			Path:    config.Path,
			Token:   config.Token,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported secrets backend %q", config.Type)
	}
}
