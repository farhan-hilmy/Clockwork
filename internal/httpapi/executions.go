package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/farhan-hilmy/clockwork/internal/executions"
)

type executionHandlers struct {
	svc   *executions.Service
	tasks *taskHandlers // ownership checks and shared error mapping
}

func (h *executionHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := executions.ListParams{TaskID: q.Get("task_id"), Status: q.Get("status"), Cursor: q.Get("cursor")}
	p.ApplicationID = principal(r).AppID() // "" for the admin: every application
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "invalid_parameter", "limit must be an integer between 1 and 200", nil)
			return
		}
		p.Limit = n
	}
	if p.Status != "" && !executions.ValidStatus(p.Status) {
		writeError(w, http.StatusBadRequest, "invalid_parameter", "status is not a valid execution status", nil)
		return
	}
	page, err := h.svc.List(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *executionHandlers) get(w http.ResponseWriter, r *http.Request) {
	e, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err == nil && !principal(r).owns(e.ApplicationID) {
		err = executions.ErrNotFound // other applications' executions do not exist for this caller
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// run queues a manual run of a task. 202: the run happens asynchronously.
// With an Idempotency-Key header, a repeated request returns the original
// execution with 200 and Idempotent-Replayed: true instead of queuing another.
func (h *executionHandlers) run(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if msg := checkIdempotencyKey(key); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_header", msg, nil)
		return
	}
	t, ok := h.tasks.owned(w, r)
	if !ok {
		return
	}
	e, replayed, err := h.svc.RunNow(r.Context(), t.ID, key)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/executions/"+e.ID)
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, e)
		return
	}
	writeJSON(w, http.StatusAccepted, e)
}

func checkIdempotencyKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) > executions.MaxIdempotencyKeyLen {
		return "Idempotency-Key must be at most 255 characters"
	}
	if strings.IndexFunc(key, func(c rune) bool { return c > unicode.MaxASCII || !unicode.IsPrint(c) }) >= 0 {
		return "Idempotency-Key must be printable ASCII"
	}
	return ""
}

func (h *executionHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, executions.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, executions.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), nil)
	default:
		h.tasks.fail(w, r, err) // maps tasks.ErrNotFound and internal errors
	}
}
