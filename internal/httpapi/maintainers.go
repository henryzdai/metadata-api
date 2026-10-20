package httpapi

import (
	"net/http"

	"metadata-api/internal/metadata"
)

func (h *Handler) maintainers(w http.ResponseWriter, r *http.Request, route route) {
	ctx := r.Context()
	id := metadata.MaintainerID(route.id)
	switch r.Method {
	case http.MethodPost:
		var input metadata.MaintainerFields
		if problem := decodeBody(w, r, &input, maintainerInputFields); problem != nil {
			writeProblem(w, problem)
			return
		}
		record, err := h.store.CreateMaintainer(ctx, input)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.Header().Set("Location", "/v1/maintainers/"+string(record.ID))
		w.Header().Set("ETag", entityTag("m1", string(record.ID), record.Revision))
		writeJSON(w, http.StatusCreated, record)
	case http.MethodGet:
		if id == "" {
			limit, after, _, problem := parseQuery(r.URL.RawQuery, false)
			if problem != nil {
				writeProblem(w, problem)
				return
			}
			page, err := h.store.ListMaintainers(ctx, metadata.MaintainerList{Limit: limit, After: metadata.MaintainerID(after)})
			if err != nil {
				writeProblem(w, fromError(err))
				return
			}
			writeJSON(w, http.StatusOK, page)
			return
		}
		record, err := h.store.GetMaintainer(ctx, id)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.Header().Set("ETag", entityTag("m1", string(record.ID), record.Revision))
		writeJSON(w, http.StatusOK, record)
	case http.MethodPut:
		var input metadata.MaintainerFields
		if problem := decodeBody(w, r, &input, maintainerInputFields); problem != nil {
			writeProblem(w, problem)
			return
		}
		input, err := metadata.NormalizeMaintainer(input)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		expected, problem := parseIfMatch(r.Header, "m1", route.id)
		if problem != nil {
			writeProblem(w, problem)
			return
		}
		record, err := h.store.UpdateMaintainer(ctx, id, input, expected)
		if err != nil {
			writeProblem(w, fromError(err))
			return
		}
		writeJSON(w, http.StatusOK, record)
	case http.MethodDelete:
		expected, problem := parseIfMatch(r.Header, "m1", route.id)
		if problem != nil {
			writeProblem(w, problem)
			return
		}
		if err := h.store.DeleteMaintainer(ctx, id, expected); err != nil {
			writeProblem(w, fromError(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
