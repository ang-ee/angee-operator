package secrets

import (
	"context"

	"github.com/ang-ee/angee-operator/internal/store"
)

// init registers env-file and openbao as store backends so they ARE part of
// the generic store registry. The live secrets construction path (FromManifest,
// backend.go) is intentionally left unchanged: these registrations exist so a
// future files-on-openbao or secrets-via-store migration is one store.Open call
// away, not to reroute current secret traffic.
//
// Both adapters implement only the core store.Store (plus Lister/Deleter); they
// deliberately do NOT implement store.Versioned. Secrets are last-write-wins —
// the CAS/etag path is a files-on-localfs concern.
func init() {
	store.Register("env-file", func(cfg store.Config) (store.Store, error) {
		path := cfg.Path
		if path == "" {
			path = ".env"
		}
		return &backendStore{backend: NewEnvFileBackend(path)}, nil
	})
	store.Register("openbao", func(cfg store.Config) (store.Store, error) {
		return &backendStore{backend: NewOpenBaoBackend(OpenBaoConfig{
			Address: cfg.Address,
			Mount:   cfg.Mount,
			Path:    cfg.Path,
			Token:   cfg.Token,
		})}, nil
	})
}

// backendStore adapts a string-valued secrets.Backend to the Blob-valued
// store.Store contract. The SecretEnvName key mapping and validateKey charset
// check remain secrets-object concerns applied by FromManifest, not by the
// store layer.
type backendStore struct {
	backend Backend
}

var (
	_ store.Store   = (*backendStore)(nil)
	_ store.Lister  = (*backendStore)(nil)
	_ store.Deleter = (*backendStore)(nil)
)

func (s *backendStore) Get(ctx context.Context, key string) (store.Blob, bool, error) {
	value, ok, err := s.backend.Get(ctx, key)
	if err != nil || !ok {
		return store.Blob{}, ok, err
	}
	return store.Blob{Bytes: []byte(value)}, true, nil
}

func (s *backendStore) Set(ctx context.Context, key string, value store.Blob) error {
	return s.backend.Set(ctx, key, string(value.Bytes))
}

func (s *backendStore) Delete(ctx context.Context, key string) error {
	return s.backend.Delete(ctx, key)
}

func (s *backendStore) List(ctx context.Context) ([]string, error) {
	return s.backend.List(ctx)
}
