package operator

import (
	"net/http"

	"github.com/ang-ee/angee-operator/api"
)

func writeServiceError(w http.ResponseWriter, err error) {
	status, body := serviceErrorResponse(err)
	writeJSON(w, status, body)
}

// serviceErrorResponse maps a service-layer error to an HTTP status and
// response body. It shares classifyServiceError with the GraphQL surface so the
// NotFound/Conflict/InvalidInput ladder lives in exactly one place. Each
// category populates only its relevant fields; the rest stay empty and are
// omitted, reproducing the historical per-category JSON shapes. For a matched
// error the message is the matched type's own Error() text (carried on
// classifiedError) so it stays correct even if a caller later %w-wraps the
// typed error; unmatched errors fall back to err.Error().
func serviceErrorResponse(err error) (int, api.ErrorResponse) {
	c := classifyServiceError(err)
	if !c.matched {
		return c.status, api.ErrorResponse{Error: err.Error()}
	}
	return c.status, api.ErrorResponse{
		Kind:   c.kind,
		Name:   c.name,
		Field:  c.field,
		Reason: c.reason,
		Error:  c.message,
	}
}
