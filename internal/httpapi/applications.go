package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/farhan-hilmy/clockwork/internal/apps"
)

// applicationHandlers manage applications and their API keys. All are admin-only.
type applicationHandlers struct {
	svc   *apps.Service
	tasks *taskHandlers // shared internal-error logging
}

func (h *applicationHandlers) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applications": list})
}

func (h *applicationHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in apps.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	a, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/applications/"+a.ID)
	writeJSON(w, http.StatusCreated, a)
}

func (h *applicationHandlers) get(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *applicationHandlers) update(w http.ResponseWriter, r *http.Request) {
	var in apps.UpdateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	a, err := h.svc.Update(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *applicationHandlers) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *applicationHandlers) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.svc.ListKeys(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// createKey returns the key itself exactly once.
func (h *applicationHandlers) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	k, err := h.svc.CreateKey(r.Context(), chi.URLParam(r, "id"), in.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, k)
}

func (h *applicationHandlers) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RevokeKey(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "keyID")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *applicationHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *apps.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case errors.Is(err, apps.ErrNotFound), errors.Is(err, apps.ErrKeyNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, apps.ErrNameTaken), errors.Is(err, apps.ErrHasTasks), errors.Is(err, apps.ErrDefaultApp):
		writeError(w, http.StatusConflict, "conflict", err.Error(), nil)
	default:
		h.tasks.internal(w, r, err)
	}
}
