package operator

import (
	"errors"
	"net/http"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
)

func writeServiceError(w http.ResponseWriter, err error) {
	status, body := serviceErrorResponse(err)
	writeJSON(w, status, body)
}

func serviceErrorResponse(err error) (int, api.ErrorResponse) {
	var notFound *service.NotFoundError
	if errors.As(err, &notFound) {
		return http.StatusNotFound, api.ErrorResponse{
			Kind:  notFound.Kind,
			Name:  notFound.Name,
			Error: notFound.Error(),
		}
	}

	var conflict *service.ConflictError
	if errors.As(err, &conflict) {
		return http.StatusConflict, api.ErrorResponse{
			Kind:   conflict.Kind,
			Name:   conflict.Name,
			Reason: conflict.Reason,
			Error:  conflict.Error(),
		}
	}

	var invalid *service.InvalidInputError
	if errors.As(err, &invalid) {
		return http.StatusBadRequest, api.ErrorResponse{
			Field:  invalid.Field,
			Reason: invalid.Reason,
			Error:  invalid.Error(),
		}
	}

	return http.StatusInternalServerError, api.ErrorResponse{Error: err.Error()}
}
