package service

import (
	"context"
	"errors"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/files"
	"github.com/ang-ee/angee-operator/internal/store"
)

// FileRead returns the raw UTF-8 contents and etag of a file at path under the
// named stack source. The relpath containment is owned by the localfs store;
// only the source key is resolved here.
func (p *Platform) FileRead(ctx context.Context, source, path string) (api.FileContent, error) {
	obj, err := p.fileObject(source, path)
	if err != nil {
		return api.FileContent{}, err
	}
	content, err := obj.Read(ctx, path)
	if err != nil {
		return api.FileContent{}, mapFileErr(err, path)
	}
	return api.FileContent{
		Path:    content.Path,
		Source:  source,
		Content: content.Content,
		Etag:    content.Etag,
	}, nil
}

// FileWrite stores content at path under the named stack source. A non-empty
// etag is an optimistic-concurrency precondition; a mismatch is a conflict.
func (p *Platform) FileWrite(ctx context.Context, source, path, content, etag string) (api.FileRef, error) {
	obj, err := p.fileObject(source, path)
	if err != nil {
		return api.FileRef{}, err
	}
	ref, err := obj.Write(ctx, path, content, etag)
	if err != nil {
		return api.FileRef{}, mapFileErr(err, path)
	}
	return api.FileRef{
		Path:   ref.Path,
		Source: source,
		Etag:   ref.Etag,
	}, nil
}

// fileObject validates inputs, resolves the source key to its on-disk dir via
// the existing source resolution, and opens a localfs-backed files object
// rooted there.
func (p *Platform) fileObject(source, path string) (*files.Object, error) {
	if source == "" {
		return nil, &InvalidInputError{Field: "source", Reason: "source is required"}
	}
	if path == "" {
		return nil, &InvalidInputError{Field: "path", Reason: "path is required"}
	}
	stack, err := p.LoadStack()
	if err != nil {
		return nil, err
	}
	src, ok := stack.Sources[source]
	if !ok {
		return nil, &NotFoundError{Kind: "source", Name: source}
	}
	base := p.sourcePath(source, src)
	// Cap the store at the same limit the files object enforces so an oversized
	// on-disk file is rejected before it is read into memory, not after.
	s, err := store.Open("localfs", store.Config{Root: base, MaxBytes: files.MaxFileBytes})
	if err != nil {
		return nil, err
	}
	obj, err := files.New(s)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// mapFileErr translates internal/files sentinels into the service error
// taxonomy so both surfaces (REST + GraphQL) map them to the right status.
func mapFileErr(err error, path string) error {
	switch {
	case errors.Is(err, files.ErrNotFound):
		return &NotFoundError{Kind: "file", Name: path}
	case errors.Is(err, files.ErrEtagMismatch):
		return &ConflictError{Kind: "file", Name: path, Reason: "etag mismatch"}
	case errors.Is(err, files.ErrNotContained):
		return &InvalidInputError{Field: "path", Reason: "path escapes the source root"}
	case errors.Is(err, files.ErrNotText):
		return &InvalidInputError{Field: "content", Reason: "not valid UTF-8 text"}
	case errors.Is(err, files.ErrTooLarge):
		return &InvalidInputError{Field: "content", Reason: "exceeds 1 MiB cap"}
	default:
		return err
	}
}
