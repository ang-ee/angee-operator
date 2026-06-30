package store

import (
	"fmt"
	"sync"
)

// Factory builds a Store from an opaque, backend-specific config map.
type Factory func(cfg Config) (Store, error)

// Config is the backend-agnostic construction input. The interpretation of
// each field is per-backend: localfs uses Root; env-file uses Path; openbao
// uses Address/Mount/Path/Token.
type Config struct {
	Root     string // localfs base dir
	Path     string // env-file file path / openbao KV path
	Address  string // openbao
	Mount    string // openbao
	Token    string // openbao
	MaxBytes int64  // optional read-side per-blob size cap; 0 means unlimited (localfs)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register installs a factory under kind. Self-registering backends call this
// from init(); duplicate kinds panic (programmer error).
func Register(kind string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if f == nil {
		panic("store: Register factory is nil")
	}
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("store: Register called twice for kind %q", kind))
	}
	registry[kind] = f
}

// Open constructs the Store registered under kind, or an error for an unknown
// kind. This is the ONE construction path through which objects request a
// store.
func Open(kind string, cfg Config) (Store, error) {
	registryMu.RLock()
	f, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown store backend %q", kind)
	}
	return f(cfg)
}
