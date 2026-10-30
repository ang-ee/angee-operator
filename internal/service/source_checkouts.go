package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/copierx"
)

// WithSourceCheckout makes a private complete repository available to use, then
// removes it even when use fails or ctx is cancelled. It neither registers a
// source nor changes the stack's source caches or manifest. Concurrent scopes
// are independent and do not hold the stack mutation lease while consumers build.
func (p *Platform) WithSourceCheckout(ctx context.Context, req api.SourceCheckoutRequest, use func(context.Context, api.SourceCheckout) error) (err error) {
	defer (operation{name: "source.checkout", title: "Checkout repository"}).annotate(&err)
	for _, input := range []struct{ field, value string }{{"repository", req.Repository}, {"revision", req.Revision}} {
		field, value := input.field, input.value
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") || strings.HasPrefix(value, "-") {
			return &InvalidInputError{Field: field, Reason: "a non-empty value without line breaks, NUL or an option prefix is required"}
		}
	}
	if use == nil {
		return &InvalidInputError{Field: "use", Reason: "checkout consumer is required"}
	}
	if _, err := p.LoadStack(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.detachedContext != nil {
		stop := context.AfterFunc(p.detachedContext, cancel)
		defer stop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	trusted, err := copierx.OpenTrustedRoot(p.root)
	if err != nil {
		return err
	}
	defer func() { _ = trusted.Close() }()
	rel := filepath.Join("run", "source-checkouts", rand.Text())
	guard, err := trusted.OpenGuardedPath(p.root, rel, nil)
	if err != nil {
		return err
	}
	defer func() { _ = guard.Close() }()
	if _, exists, err := guard.Lstat(); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("temporary checkout path already exists")
	}
	if err := guard.MkdirAll(0o700); err != nil {
		return err
	}
	defer func() {
		if cleanup := guard.RemoveAll(); cleanup != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary checkout: %w", cleanup))
		}
	}()
	if err := guard.VerifyPathIdentity(filepath.Join(p.root, rel)); err != nil {
		return err
	}
	path := filepath.Join(rel, "repository")
	commit, err := p.gitClient().CompleteCheckout(ctx, req.Repository, filepath.Join(p.root, path), req.Revision)
	if err != nil {
		return gitFailure(err, gitStep{action: "preparing", object: "repository checkout", remote: req.Repository, cause: CauseCloneFailed})
	}
	if err := guard.VerifyPathIdentity(filepath.Join(p.root, rel)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return use(ctx, api.SourceCheckout{Path: filepath.ToSlash(path), Revision: commit})
}
