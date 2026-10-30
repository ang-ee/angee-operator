package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ang-ee/angee-operator/api"
)

func sourceCheckoutFixture(t *testing.T, options ...Option) (*Platform, api.SourceCheckoutRequest) {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "angee.yaml"), "version: 1\nkind: stack\nname: checkout-test\n")
	repository := t.TempDir()
	runGit(t, repository, "init", "-b", "main")
	runGit(t, repository, "config", "user.name", "Checkout Test")
	runGit(t, repository, "config", "user.email", "checkout@example.com")
	mustWriteFile(t, filepath.Join(repository, "pyproject.toml"), "[project]\nname = 'sample'\nversion = '1.0'\n")
	runGit(t, repository, "add", ".")
	runGit(t, repository, "commit", "-m", "initial")
	p, err := New(root, options...)
	if err != nil {
		t.Fatal(err)
	}
	return p, api.SourceCheckoutRequest{Repository: repository, Revision: "main"}
}

func TestSourceCheckoutScopeCleanupAndIndependentLifetimes(t *testing.T) {
	p, req := sourceCheckoutFixture(t)
	failure := errors.New("metadata build failed")
	for _, result := range []error{nil, failure} {
		var path string
		err := p.WithSourceCheckout(t.Context(), req, func(ctx context.Context, checkout api.SourceCheckout) error {
			if filepath.IsAbs(checkout.Path) || !filepath.IsLocal(checkout.Path) || checkout.Revision == "" {
				t.Fatalf("checkout = %#v", checkout)
			}
			path = filepath.Join(p.root, checkout.Path)
			if _, err := os.Stat(filepath.Join(path, "pyproject.toml")); err != nil {
				t.Fatal(err)
			}
			// Scopes neither serialize independent builds nor hold the stack's
			// mutation lease while a caller is using the files.
			_, release, err := p.beginMutation(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			release()
			err = p.WithSourceCheckout(ctx, req, func(_ context.Context, other api.SourceCheckout) error {
				if other.Path == checkout.Path {
					t.Fatal("scopes share a checkout")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("inner scope removed outer checkout", err)
			}
			return result
		})
		if !errors.Is(err, result) {
			t.Fatalf("scope error = %v, want %v", err, result)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("checkout remains: %v", err)
		}
	}
}

func TestSourceCheckoutCancellationAndInvalidRequests(t *testing.T) {
	daemon, stop := context.WithCancel(t.Context())
	defer stop()
	p, req := sourceCheckoutFixture(t, WithDetachedContext(daemon))
	var path string
	err := p.WithSourceCheckout(t.Context(), req, func(ctx context.Context, checkout api.SourceCheckout) error {
		path = filepath.Join(p.root, checkout.Path)
		stop()
		<-ctx.Done()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("daemon-cancelled checkout remains: %v", err)
	}
	p, req = sourceCheckoutFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.WithSourceCheckout(ctx, req, func(context.Context, api.SourceCheckout) error {
		t.Fatal("cancelled request entered callback")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	for _, invalid := range []api.SourceCheckoutRequest{{}, {Repository: req.Repository, Revision: "--exec=bad"}, {Repository: req.Repository, Revision: "missing"}, {Repository: "unsupported://bad", Revision: "main"}} {
		if err := p.WithSourceCheckout(t.Context(), invalid, func(context.Context, api.SourceCheckout) error {
			t.Fatal("invalid request entered callback")
			return nil
		}); err == nil {
			t.Fatalf("accepted %#v", invalid)
		}
	}
	entries, err := os.ReadDir(filepath.Join(p.root, "run/source-checkouts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed checkout leaked files: %v %v", entries, err)
	}
}

func TestSourceCheckoutRefusesSymlinkedRunDirectory(t *testing.T) {
	p, req := sourceCheckoutFixture(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(p.root, "run")); err != nil {
		t.Skip(err)
	}
	if err := p.WithSourceCheckout(t.Context(), req, func(context.Context, api.SourceCheckout) error { t.Fatal("symlinked root accepted"); return nil }); err == nil {
		t.Fatal("symlinked root accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: %v %v", entries, err)
	}
}
