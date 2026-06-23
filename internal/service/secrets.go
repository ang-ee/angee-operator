package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/secrets"
	"github.com/ang-ee/angee-operator/internal/substitute"
)

// secretNamePattern bounds names accepted by the operator-side CRUD API.
// Same charset env-file's validateKey accepts; rejects path-traversal,
// shell metacharacters, and anything OpenBao would surprise on. Length
// cap (256) keeps the request body bounded.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

// secretNameAlphaNum enforces "at least one alphanumeric character"
// independently of secretNamePattern. Without this, names like `..`,
// `--`, `_`, etc. all map through substitute.SecretEnvName (which
// rewrites non-alphanum to `_` and trims trailing underscores) to the
// same storage key `ANGEE_SECRET`, silently colliding distinct
// manifest entries on env-file backends. Reject the degenerate forms
// at the API boundary so callers see a typed error instead.
var secretNameAlphaNum = regexp.MustCompile(`[A-Za-z0-9]`)

// maxSecretValueBytes caps a single secret's value size. The REST body
// limit (1 MiB) is generous for typical secrets; a Platform-level cap
// catches accidental file-paste (PEM bundles, base64 blobs) before it
// hits the backend.
const maxSecretValueBytes = 64 * 1024

func validateSecretName(name string) error {
	if name == "" {
		return &InvalidInputError{Field: "name", Reason: "secret name is required"}
	}
	if !secretNamePattern.MatchString(name) {
		return &InvalidInputError{Field: "name", Reason: "secret name must match ^[A-Za-z0-9._-]{1,256}$"}
	}
	if !secretNameAlphaNum.MatchString(name) {
		return &InvalidInputError{Field: "name", Reason: "secret name must contain at least one alphanumeric character"}
	}
	return nil
}

// SecretsList returns metadata for every secret declared in the stack
// manifest. Undeclared backend keys are intentionally not enumerated —
// see docs/guide/concepts.md "Secrets" for the rationale.
func (p *Platform) SecretsList(ctx context.Context, q query.Args) ([]api.SecretRef, int, error) {
	if err := query.Validate(q, queryfields.Secret); err != nil {
		return nil, 0, invalidQueryError(err)
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, 0, err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return nil, 0, err
	}
	refs := make([]api.SecretRef, 0, len(stack.Secrets))
	for _, name := range sortedKeys(stack.Secrets) {
		ref, err := buildSecretRef(ctx, backend, stack, name)
		if err != nil {
			return nil, 0, err
		}
		refs = append(refs, ref)
	}
	page, total := query.Apply(refs, q, queryfields.Secret)
	return page, total, nil
}

// SecretGet returns metadata for one secret. Works on both declared and
// undeclared names; the response's `Declared` flag distinguishes.
func (p *Platform) SecretGet(ctx context.Context, name string) (api.SecretRef, error) {
	if err := validateSecretName(name); err != nil {
		return api.SecretRef{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.SecretRef{}, err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return api.SecretRef{}, err
	}
	return buildSecretRef(ctx, backend, stack, name)
}

// SecretValue returns the resolved value for one secret. Privileged
// read: separated from SecretGet so the value-returning path is obvious
// in audit logs and code review.
//
// Returns NotFoundError when the backend has no value for the name —
// callers should not be able to distinguish "declared but no value" from
// "undeclared and no value" without first calling SecretGet (which is
// less sensitive).
func (p *Platform) SecretValue(ctx context.Context, name string) (api.SecretValueResponse, error) {
	if err := validateSecretName(name); err != nil {
		return api.SecretValueResponse{}, err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.SecretValueResponse{}, err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return api.SecretValueResponse{}, err
	}
	value, ok, err := backend.Get(ctx, name)
	if err != nil {
		return api.SecretValueResponse{}, fmt.Errorf("get secret %q: %w", name, err)
	}
	if !ok {
		return api.SecretValueResponse{}, &NotFoundError{Kind: "secret", Name: name}
	}
	return api.SecretValueResponse{Name: name, Value: value}, nil
}

// SecretSet upserts the value for one secret. Accepts any valid name
// (declared or not). Empty values are rejected — use SecretDelete to
// remove instead.
func (p *Platform) SecretSet(ctx context.Context, name, value string) (api.SecretRef, error) {
	if err := validateSecretName(name); err != nil {
		return api.SecretRef{}, err
	}
	if value == "" {
		return api.SecretRef{}, &InvalidInputError{Field: "value", Reason: "value is empty; use secretDelete to remove"}
	}
	if len(value) > maxSecretValueBytes {
		return api.SecretRef{}, &InvalidInputError{Field: "value", Reason: fmt.Sprintf("value exceeds %d byte cap", maxSecretValueBytes)}
	}
	stack, err := p.LoadStack()
	if err != nil {
		return api.SecretRef{}, err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return api.SecretRef{}, err
	}
	if err := backend.Set(ctx, name, value); err != nil {
		return api.SecretRef{}, fmt.Errorf("set secret %q: %w", name, err)
	}
	return buildSecretRef(ctx, backend, stack, name)
}

// SecretDelete removes the backend entry for one secret. Idempotent:
// deleting a non-existent name returns nil. The declared manifest
// entry (if any) is left alone — only the backend value is removed.
func (p *Platform) SecretDelete(ctx context.Context, name string) error {
	if err := validateSecretName(name); err != nil {
		return err
	}
	stack, err := p.LoadStack()
	if err != nil {
		return err
	}
	backend, err := secrets.FromManifest(p.root, stack.SecretsBackend, substitute.SecretEnvName)
	if err != nil {
		return err
	}
	if err := backend.Delete(ctx, name); err != nil {
		// Most backends treat delete-missing as success; only surface
		// real errors.
		if !errors.Is(err, ErrNoOp) {
			return fmt.Errorf("delete secret %q: %w", name, err)
		}
	}
	return nil
}

// ErrNoOp is reserved for backends that explicitly distinguish
// "nothing was removed because the key did not exist" from real errors.
// Today neither backend uses it; reserved so the SecretDelete contract
// can grow without touching callers.
var ErrNoOp = errors.New("no-op")

func buildSecretRef(ctx context.Context, backend secrets.Backend, stack *manifest.Stack, name string) (api.SecretRef, error) {
	_, hasValue, err := backend.Get(ctx, name)
	if err != nil {
		return api.SecretRef{}, fmt.Errorf("get secret %q: %w", name, err)
	}
	ref := api.SecretRef{
		Name:     name,
		HasValue: hasValue,
		EnvVar:   substitute.SecretEnvName(name),
	}
	if spec, ok := stack.Secrets[name]; ok {
		ref.Declared = true
		ref.Required = spec.Required
		ref.Generated = spec.Generated
		ref.Import = spec.Import
	}
	return ref, nil
}
