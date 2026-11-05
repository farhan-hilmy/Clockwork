package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

type taskHandlers struct {
	svc *tasks.Service
	log *slog.Logger
}

func (h *taskHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := tasks.ListParams{
		ApplicationID: q.Get("application_id"),
		Query:         q.Get("q"),
		Cursor:        q.Get("cursor"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "limit must be an integer between 1 and 200", nil)
			return
		}
		p.Limit = n
	}
	if v := q.Get("enabled"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "enabled must be true or false", nil)
			return
		}
		p.Enabled = &b
	}
	page, err := h.svc.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *taskHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in tasks.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	t, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/tasks/"+t.ID)
	writeJSON(w, http.StatusCreated, t)
}

func (h *taskHandlers) get(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *taskHandlers) update(w http.ResponseWriter, r *http.Request) {
	var in tasks.UpdateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	t, err := h.svc.Update(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *taskHandlers) setEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := h.svc.SetEnabled(r.Context(), chi.URLParam(r, "id"), enabled)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func (h *taskHandlers) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fail maps domain errors to HTTP responses.
func (h *taskHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	if ve, ok := tasks.IsValidation(err); ok {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
		return
	}
	switch {
	case errors.Is(err, tasks.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, tasks.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
	default:
		h.internal(w, r, err)
	}
}

func (h *taskHandlers) internal(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("request failed", "err", err, "path", r.URL.Path, "request_id", middleware.GetReqID(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal", "internal server error", nil)
}
