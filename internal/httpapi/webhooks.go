package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
	"github.com/farhan-hilmy/clockwork/internal/webhooks"
)

// webhookHandlers manage webhook endpoints and their delivery logs. An
// application manages its own endpoints; the admin manages anyone's.
type webhookHandlers struct {
	svc   *webhooks.Service
	tasks *taskHandlers // shared internal-error logging
}

func (h *webhookHandlers) list(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("application_id")
	if p := principal(r); !p.Admin {
		appID = p.App.ID
	}
	list, err := h.svc.List(r.Context(), appID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": list})
}

// create returns the signing secret exactly once.
func (h *webhookHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in webhooks.EndpointInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if p := principal(r); !p.Admin {
		if in.ApplicationID != "" && in.ApplicationID != p.App.ID {
			h.fail(w, r, &webhooks.ValidationError{Fields: map[string]string{"application_id": "an application can only create webhooks for itself; omit this field"}})
			return
		}
		in.ApplicationID = p.App.ID
	} else if in.ApplicationID == "" {
		in.ApplicationID = tasks.DefaultApplicationID
	}
	e, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/webhooks/"+e.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, e)
}

// owned loads an endpoint the caller may see; others answer 404.
func (h *webhookHandlers) owned(w http.ResponseWriter, r *http.Request) (webhooks.Endpoint, bool) {
	e, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err == nil && !principal(r).owns(e.ApplicationID) {
		err = webhooks.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return webhooks.Endpoint{}, false
	}
	return e, true
}

func (h *webhookHandlers) get(w http.ResponseWriter, r *http.Request) {
	if e, ok := h.owned(w, r); ok {
		writeJSON(w, http.StatusOK, e)
	}
}

func (h *webhookHandlers) update(w http.ResponseWriter, r *http.Request) {
	var in webhooks.EndpointUpdate
	if !decodeJSON(w, r, &in) {
		return
	}
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	e, err := h.svc.Update(r.Context(), e.ID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *webhookHandlers) delete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), e.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *webhookHandlers) rotateSecret(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	e, err := h.svc.RotateSecret(r.Context(), e.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, e)
}

// test queues a webhook.test delivery. 202: it is sent asynchronously.
func (h *webhookHandlers) test(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	d, err := h.svc.SendTest(r.Context(), e.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (h *webhookHandlers) deliveries(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p := webhooks.DeliveryParams{EndpointID: e.ID, Status: q.Get("status"), Cursor: q.Get("cursor")}
	switch p.Status {
	case "", webhooks.StatusPending, webhooks.StatusDelivered, webhooks.StatusFailed:
	default:
		writeError(w, http.StatusBadRequest, "invalid_parameter", "status must be pending, delivered or failed", nil)
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "limit must be an integer between 1 and 200", nil)
			return
		}
		p.Limit = n
	}
	page, err := h.svc.Deliveries(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *webhookHandlers) delivery(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	d, err := h.svc.Delivery(r.Context(), e.ID, chi.URLParam(r, "deliveryID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *webhookHandlers) redeliver(w http.ResponseWriter, r *http.Request) {
	e, ok := h.owned(w, r)
	if !ok {
		return
	}
	d, err := h.svc.Redeliver(r.Context(), e.ID, chi.URLParam(r, "deliveryID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (h *webhookHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *webhooks.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "one or more fields are invalid", ve.Fields)
	case errors.Is(err, webhooks.ErrNotFound), errors.Is(err, webhooks.ErrDeliveryNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	default:
		h.tasks.internal(w, r, err)
	}
}
