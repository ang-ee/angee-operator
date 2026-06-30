package operator

import (
	"errors"
	"net/http"

	"github.com/ang-ee/angee-operator/internal/service"
)

// classifiedError is the single decoded form of a service-layer error, shared
// by the REST (serviceErrorResponse) and GraphQL (formatGraphQLError) surfaces
// so both map the service error taxonomy through one ladder instead of two
// parallel ones. status is the HTTP status the category maps to (also used as
// the category discriminator); matched is false when err is none of the known
// service types, in which case status is 500 and the caller renders its
// error-message-only default.
type classifiedError struct {
	status  int
	kind    string
	name    string
	field   string
	reason  string
	message string // the matched error's own Error() text, robust to %w-wrapping
	matched bool
}

// classifyServiceError walks the service error taxonomy exactly once. It is the
// one place the NotFound/Conflict/InvalidInput ladder lives; REST and GraphQL
// both call it and shape their own response from the result.
func classifyServiceError(err error) classifiedError {
	var notFound *service.NotFoundError
	if errors.As(err, &notFound) {
		return classifiedError{status: http.StatusNotFound, kind: notFound.Kind, name: notFound.Name, message: notFound.Error(), matched: true}
	}

	var conflict *service.ConflictError
	if errors.As(err, &conflict) {
		return classifiedError{status: http.StatusConflict, kind: conflict.Kind, name: conflict.Name, reason: conflict.Reason, message: conflict.Error(), matched: true}
	}

	var invalid *service.InvalidInputError
	if errors.As(err, &invalid) {
		return classifiedError{status: http.StatusBadRequest, field: invalid.Field, reason: invalid.Reason, message: invalid.Error(), matched: true}
	}

	return classifiedError{status: http.StatusInternalServerError}
}
