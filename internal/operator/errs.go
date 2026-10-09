package operator

import (
	"net/http"
)

// writeServiceError writes err as a REST error body with the status its code
// maps to, sharing classifyServiceError with the GraphQL surface, and hands
// the failure to the request log.
func writeServiceError(w http.ResponseWriter, err error) {
	c := classifyServiceError(err)
	recordFailure(w, c)
	writeJSON(w, c.status, c.errorResponse())
}

// failureRecorder is implemented by the request-logging response writer, which
// logs a recorded failure once the handler returns.
type failureRecorder interface {
	recordFailure(c classifiedError)
}

// recordFailure hands c to the request-logging writer under w, if any.
func recordFailure(w http.ResponseWriter, c classifiedError) {
	for w != nil {
		if recorder, ok := w.(failureRecorder); ok {
			recorder.recordFailure(c)
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}
