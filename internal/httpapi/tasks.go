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

// shellForbidden is returned when an application tries to use the shell runner.
// Shell tasks run arbitrary commands on the Clockwork host, so only the operator may create them.
var shellForbidden = &tasks.ValidationError{Fields: map[string]string{"runner.type": "applications cannot use the shell runner"}}

func (h *taskHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := tasks.ListParams{
		ApplicationID: q.Get("application_id"),
		Query:         q.Get("q"),
		Cursor:        q.Get("cursor"),
	}
	if app := principal(r).AppID(); app != "" {
		p.ApplicationID = app // an application only ever lists its own tasks
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
	if p := principal(r); !p.Admin {
		if in.ApplicationID != "" && in.ApplicationID != p.App.ID {
			h.fail(w, r, &tasks.ValidationError{Fields: map[string]string{"application_id": "an application can only create tasks for itself; omit this field"}})
			return
		}
		if in.Runner.Type == tasks.RunnerShell {
			h.fail(w, r, shellForbidden)
			return
		}
		in.ApplicationID = p.App.ID
	}
	t, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/tasks/"+t.ID)
	writeJSON(w, http.StatusCreated, t)
}

// owned loads a task and checks the caller may see it. A task of another
// application is reported as not found, so its existence does not leak.
func (h *taskHandlers) owned(w http.ResponseWriter, r *http.Request) (tasks.Task, bool) {
	t, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err == nil && !principal(r).owns(t.ApplicationID) {
		err = tasks.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return tasks.Task{}, false
	}
	return t, true
}

func (h *taskHandlers) get(w http.ResponseWriter, r *http.Request) {
	if t, ok := h.owned(w, r); ok {
		writeJSON(w, http.StatusOK, t)
	}
}

func (h *taskHandlers) update(w http.ResponseWriter, r *http.Request) {
	var in tasks.UpdateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	t, ok := h.owned(w, r)
	if !ok {
		return
	}
	if !principal(r).Admin && in.Runner.Set && in.Runner.Value.Type == tasks.RunnerShell {
		h.fail(w, r, shellForbidden)
		return
	}
	t, err := h.svc.Update(r.Context(), t.ID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *taskHandlers) setEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := h.owned(w, r)
		if !ok {
			return
		}
		t, err := h.svc.SetEnabled(r.Context(), t.ID, enabled)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}
}

func (h *taskHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t, ok := h.owned(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), t.ID); err != nil {
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
