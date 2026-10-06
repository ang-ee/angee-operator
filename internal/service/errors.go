package service

import (
	"errors"
	"fmt"

	"github.com/ang-ee/angee-operator/internal/query"
)

// NoManifestError reports that a root holds no angee.yaml. It unwraps to the
// open error, which matches os.ErrNotExist, so callers that branch on the
// missing-manifest case keep working; check it with errors.Is, since
// os.IsNotExist does not unwrap.
type NoManifestError struct {
	Root string
	Err  error
}

func (e *NoManifestError) Error() string {
	return fmt.Sprintf("no angee.yaml found in %s: run `angee init` to create a stack here, or pass --root with the ANGEE_ROOT that owns this checkout", e.Root)
}

func (e *NoManifestError) Unwrap() error { return e.Err }

type NotFoundError struct {
	Kind string
	Name string
}

func (e *NotFoundError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("%s not found", e.Kind)
	}
	return fmt.Sprintf("%s %q is not declared", e.Kind, e.Name)
}

type ConflictError struct {
	Kind   string
	Name   string
	Reason string
}

func (e *ConflictError) Error() string {
	switch {
	case e.Reason != "" && e.Name != "":
		return fmt.Sprintf("%s %s conflicts: %s", e.Kind, e.Name, e.Reason)
	case e.Name != "":
		return fmt.Sprintf("%s %s conflicts", e.Kind, e.Name)
	default:
		return fmt.Sprintf("%s conflicts", e.Kind)
	}
}

type InvalidInputError struct {
	Field  string
	Reason string
}

func (e *InvalidInputError) Error() string {
	if e.Field == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Reason)
}

// invalidQueryError maps a *query.FieldError (unknown filter/sort field) to an
// *InvalidInputError so the surface layer renders it as 400 / a GraphQL error.
// Any other error passes through unchanged.
func invalidQueryError(err error) error {
	var fe *query.FieldError
	if errors.As(err, &fe) {
		return &InvalidInputError{Field: fe.Field, Reason: "unknown " + fe.Kind + " field"}
	}
	return err
}
