package httpapi

import (
	"net/http"

	"metadata-api/internal/metadata"
)

func (h *Handler) applications(w http.ResponseWriter, r *http.Request, route route) {
	ctx := r.Context()
	id := metadata.ApplicationID(route.id)
	switch r.Method {
	case http.MethodPost:
		var input metadata.CreateApplicationInput
		if problem := decodeBody(w, r, &input, applicationCreateFields); problem != nil {
			writeProblem(w, problem)
			return
		}
		record, err := h.store.CreateApplication(ctx, input)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.Header().Set("Location", "/v1/applications/"+string(record.ID))
		w.Header().Set("ETag", entityTag("a1", string(record.ID), record.Revision))
		writeJSON(w, http.StatusCreated, record)
	case http.MethodGet:
		if id == "" {
			limit, after, owner, problem := parseQuery(r.URL.RawQuery, true)
			if problem != nil {
				writeProblem(w, problem)
				return
			}
			page, err := h.store.ListApplications(ctx, metadata.ApplicationList{
				Limit: limit, After: metadata.ApplicationID(after), MaintainerID: owner,
			})
			if err != nil {
				writeProblem(w, fromError(err))
				return
			}
			writeJSON(w, http.StatusOK, page)
			return
		}
		record, err := h.store.GetApplication(ctx, id)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.Header().Set("ETag", entityTag("a1", string(record.ID), record.Revision))
		writeJSON(w, http.StatusOK, record)
	case http.MethodPut:
		var input metadata.ApplicationFields
		if problem := decodeBody(w, r, &input, applicationInputFields); problem != nil {
			writeProblem(w, problem)
			return
		}
		input, err := metadata.NormalizeApplication(input)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		expected, problem := parseIfMatch(r.Header, "a1", route.id)
		if problem != nil {
			writeProblem(w, problem)
			return
		}
		record, err := h.store.UpdateApplication(ctx, id, input, expected)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		writeJSON(w, http.StatusOK, record)
	case http.MethodDelete:
		expected, problem := parseIfMatch(r.Header, "a1", route.id)
		if problem != nil {
			writeProblem(w, problem)
			return
		}
		if err := h.store.DeleteApplication(ctx, id, expected); err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
