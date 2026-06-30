package store

import (
	"context"
	"testing"
)

func TestOpenUnknownKind(t *testing.T) {
	if _, err := Open("does-not-exist", Config{}); err == nil {
		t.Fatal("expected error for unknown store backend, got nil")
	}
}

func TestRegisterOpenRoundtrip(t *testing.T) {
	kind := "registry_test_fake"
	Register(kind, func(cfg Config) (Store, error) {
		return &memStore{root: cfg.Root, data: map[string][]byte{}}, nil
	})

	s, err := Open(kind, Config{Root: "abc"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set(context.Background(), "k", Blob{Bytes: []byte("v")}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(context.Background(), "k")
	if err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if string(got.Bytes) != "v" {
		t.Fatalf("Get = %q, want %q", got.Bytes, "v")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	kind := "registry_test_dup"
	Register(kind, func(cfg Config) (Store, error) { return nil, nil })
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate Register, got none")
		}
	}()
	Register(kind, func(cfg Config) (Store, error) { return nil, nil })
}

type memStore struct {
	root string
	data map[string][]byte
}

func (m *memStore) Get(_ context.Context, key string) (Blob, bool, error) {
	b, ok := m.data[key]
	if !ok {
		return Blob{}, false, nil
	}
	return Blob{Bytes: b}, true, nil
}

func (m *memStore) Set(_ context.Context, key string, value Blob) error {
	m.data[key] = value.Bytes
	return nil
}
