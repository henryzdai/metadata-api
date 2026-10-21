package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"metadata-api/internal/metadata"
)

type problem struct {
	status  int
	code    string
	message string
	fields  map[string]string
}

func fromError(err error) *problem {
	var domain *metadata.Error
	if !errors.As(err, &domain) {
		return &problem{
			status:  http.StatusInternalServerError,
			code:    "internal_error",
			message: "An internal error occurred.",
		}
	}
	status := map[metadata.ErrorCode]int{
		metadata.InvalidID:                http.StatusBadRequest,
		metadata.ValidationFailed:         http.StatusUnprocessableEntity,
		metadata.NotFound:                 http.StatusNotFound,
		metadata.DuplicateEmail:           http.StatusConflict,
		metadata.InvalidReference:         http.StatusUnprocessableEntity,
		metadata.MaintainerInUse:          http.StatusConflict,
		metadata.PreconditionFailed:       http.StatusPreconditionFailed,
		metadata.ResourceCapacityExceeded: http.StatusServiceUnavailable,
		metadata.Unavailable:              http.StatusServiceUnavailable,
		metadata.InternalError:            http.StatusInternalServerError,
	}[domain.Code]
	if status == 0 {
		return &problem{
			status:  http.StatusInternalServerError,
			code:    "internal_error",
			message: "An internal error occurred.",
		}
	}
	return &problem{
		status:  status,
		code:    string(domain.Code),
		message: domain.Message,
		fields:  domain.Fields,
	}
}

func writeProblem(w http.ResponseWriter, p *problem) {
	type errorDetails struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields,omitempty"`
	}
	writeJSON(w, p.status, struct {
		Error     errorDetails `json:"error"`
		RequestID string       `json:"request_id"`
	}{
		Error: errorDetails{
			Code:    p.code,
			Message: p.message,
			Fields:  p.fields,
		},
		RequestID: w.Header().Get("X-Request-ID"),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		writeProblem(w, fromError(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
